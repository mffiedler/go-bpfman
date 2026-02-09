package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
	"github.com/frobware/go-bpfman/outcome"
)

// unload removes a program's kernel-side artefacts. If persisted is
// true, the plan also deletes the program record from the store.
//
// This is the internal workhorse; it takes data directly, bypassing
// the store lookup and dependency checks that the public Unload
// performs.
func (m *Manager) unload(ctx context.Context, kernelID uint32, programName string, links []bpfman.LinkRecord, persisted bool) error {
	progPinPath := m.fsctx.BPFFS().ProgPinPath(kernelID)
	mapsDir := m.fsctx.BPFFS().MapPinDir(kernelID)
	linksDir := m.fsctx.BPFFS().LinkPinDir(kernelID)

	plan := m.unloadPlan(kernelID, programName, progPinPath, mapsDir, linksDir, links, persisted)
	begin := func(_ context.Context) *operation.RunState {
		return m.beginOp(ctx)
	}
	if err := operation.Run0(ctx, begin, m.executor, plan); err != nil {
		return wrapOpErr(err)
	}
	return nil
}

// Unload removes a BPF program, its links, and metadata.
//
// Preflight failures (store lookup, dependency check) return plain
// errors. Execution failures return *ManagerError with the full
// operation outcome.
func (m *Manager) Unload(ctx context.Context, kernelID uint32) error {
	// FETCH: Get metadata and links (for link cleanup)
	progSpec, err := m.getProgram(ctx, kernelID)
	if err != nil {
		// Distinguish "not found" from "not managed" by checking kernel.
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			if _, kerr := m.kernel.GetProgramByID(ctx, kernelID); kerr == nil {
				return bpfman.ErrProgramNotManaged{ID: kernelID}
			}
		}
		return err
	}

	programName := progSpec.Meta.Name

	// FETCH: Check for dependent programs (map sharing)
	// Programs that share maps with this program must be unloaded first.
	depCount, err := m.store.CountDependentPrograms(ctx, kernelID)
	if err != nil {
		return fmt.Errorf("check dependent programs for %d: %w", kernelID, err)
	}
	if depCount > 0 {
		return fmt.Errorf("cannot unload program %d: %d dependent program(s) share its maps; unload dependents first", kernelID, depCount)
	}

	links, err := m.store.ListLinksByProgram(ctx, kernelID)
	if err != nil {
		return fmt.Errorf("list links for program %d: %w", kernelID, err)
	}

	// FETCH: Collect dispatcher keys for any TC/XDP links before
	// the unload actions delete them from the store. We need these
	// to check whether the dispatchers are now empty afterwards.
	dispatcherKeys := m.collectDispatcherKeys(ctx, links)

	m.logger.InfoContext(ctx, "unloading program", "kernel_id", kernelID, "links", len(links))

	if err := m.unload(ctx, kernelID, programName, links, true); err != nil {
		return err
	}

	// Clean up any dispatchers left empty by the link removal.
	m.cleanupEmptyDispatchers(ctx, dispatcherKeys)

	m.logger.InfoContext(ctx, "unloaded program", "kernel_id", kernelID)
	return nil
}

// unloadPlan builds the operation plan for unloading a program and
// its associated links.
//
// Order: detach each link, remove links directory, unload program
// pin, unload maps directory, delete program metadata (if persisted),
// best-effort bytecode removal.
//
// When persisted is false the delete-program node is omitted. This
// is used during batch Load cleanup where programs have not yet been
// saved to the store.
func (m *Manager) unloadPlan(kernelID uint32, programName, progPinPath, mapsDir, linksDir string, links []bpfman.LinkRecord, persisted bool) operation.Plan {
	var nodes []operation.Node

	for _, link := range links {
		if link.PinPath == nil {
			continue
		}
		pinPath := link.PinPath.String()
		linkID := uint32(link.ID)
		nodes = append(nodes, operation.Do(
			"detach-link", outcome.StepKindKernelDetachLink,
			fmt.Sprintf("%d", link.ID),
			func(ctx context.Context, _ *operation.Bindings) error {
				return m.executor.Execute(ctx, action.DetachLink{PinPath: pinPath})
			},
			operation.DetailsFn(func(_ *operation.Bindings) any {
				return outcome.LinkDetails{LinkID: linkID, PinPath: pinPath}
			}),
		))
	}

	nodes = append(nodes, operation.Do(
		"remove-links-dir", outcome.StepKindKernelRemovePin, linksDir,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.RemovePin{Path: linksDir})
		},
	))

	nodes = append(nodes, operation.Do(
		"unload-prog", outcome.StepKindKernelUnload, programName,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.UnloadProgram{PinPath: progPinPath})
		},
		operation.DetailsFn(func(_ *operation.Bindings) any {
			return outcome.ProgramDetails{KernelID: kernelID, PinPath: progPinPath}
		}),
	))

	nodes = append(nodes, operation.Do(
		"unload-maps", outcome.StepKindKernelUnload, programName,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.UnloadProgram{PinPath: mapsDir})
		},
		operation.DetailsFn(func(_ *operation.Bindings) any {
			return outcome.ProgramDetails{KernelID: kernelID, MapsDirPath: mapsDir}
		}),
	))

	if persisted {
		nodes = append(nodes, operation.Do(
			"delete-program", outcome.StepKindStoreDeleteProgram, programName,
			func(ctx context.Context, _ *operation.Bindings) error {
				return m.executor.Execute(ctx, action.DeleteProgram{KernelID: kernelID})
			},
			operation.DetailsFn(func(_ *operation.Bindings) any {
				return outcome.ProgramDetails{KernelID: kernelID}
			}),
		))
	}

	nodes = append(nodes, operation.Try(
		"fs-remove-program", outcome.StepKindFSRemoveProgram, programName,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.RemoveProgramDir{KernelID: kernelID})
		},
	))

	return operation.Build(nodes...)
}
