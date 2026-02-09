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

// Unload removes a BPF program, its links, and metadata.
//
// Pattern: FETCH -> COMPUTE (plan) -> EXECUTE (interpreter)
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) Unload(ctx context.Context, kernelID uint32) error {
	rs := m.beginOp(ctx)

	fail := func(primaryErr error) error {
		rs.Outcome.PrimaryError = primaryErr.Error()
		rs.Rec.Finalise()
		return &ManagerError{Outcome: *rs.Outcome, Cause: primaryErr}
	}

	// FETCH: Get metadata and links (for link cleanup)
	progSpec, err := m.getProgram(ctx, kernelID)
	if err != nil {
		primaryErr := err
		// Distinguish "not found" from "not managed" by checking kernel.
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			if _, kerr := m.kernel.GetProgramByID(ctx, kernelID); kerr == nil {
				primaryErr = bpfman.ErrProgramNotManaged{ID: kernelID}
			}
		}
		rs.Rec.FailStep(outcome.StepKindPreflight, fmt.Sprintf("%d", kernelID), primaryErr)
		return fail(primaryErr)
	}

	programName := progSpec.Meta.Name

	// FETCH: Check for dependent programs (map sharing)
	// Programs that share maps with this program must be unloaded first.
	depCount, err := m.store.CountDependentPrograms(ctx, kernelID)
	if err != nil {
		primaryErr := fmt.Errorf("check dependent programs for %d: %w", kernelID, err)
		rs.Rec.FailStep(outcome.StepKindPreflight, programName, primaryErr)
		return fail(primaryErr)
	}
	if depCount > 0 {
		primaryErr := fmt.Errorf("cannot unload program %d: %d dependent program(s) share its maps; unload dependents first", kernelID, depCount)
		rs.Rec.FailStep(outcome.StepKindPreflight, programName, primaryErr)
		return fail(primaryErr)
	}

	links, err := m.store.ListLinksByProgram(ctx, kernelID)
	if err != nil {
		primaryErr := fmt.Errorf("list links for program %d: %w", kernelID, err)
		rs.Rec.FailStep(outcome.StepKindPreflight, programName, primaryErr)
		return fail(primaryErr)
	}

	// FETCH: Collect dispatcher keys for any TC/XDP links before
	// the unload actions delete them from the store. We need these
	// to check whether the dispatchers are now empty afterwards.
	dispatcherKeys := m.collectDispatcherKeys(ctx, links)

	// COMPUTE: Build paths from convention (kernel ID + bpffs root)
	progPinPath := m.fsctx.BPFFS().ProgPinPath(kernelID)
	mapsDir := m.fsctx.BPFFS().MapPinDir(kernelID)
	linksDir := m.fsctx.BPFFS().LinkPinDir(kernelID)

	m.logger.InfoContext(ctx, "unloading program", "kernel_id", kernelID, "links", len(links))

	// COMPUTE + EXECUTE via plan interpreter.
	plan := m.unloadPlan(kernelID, programName, progPinPath, mapsDir, linksDir, links)
	begin := func(_ context.Context) *operation.RunState { return rs }
	if err := operation.Run0(ctx, begin, m.executor, plan); err != nil {
		return wrapOpErr(err)
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
// pin, unload maps directory, delete program metadata, best-effort
// bytecode removal.
func (m *Manager) unloadPlan(kernelID uint32, programName, progPinPath, mapsDir, linksDir string, links []bpfman.LinkRecord) operation.Plan {
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

	nodes = append(nodes, operation.Do(
		"delete-program", outcome.StepKindStoreDeleteProgram, programName,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.DeleteProgram{KernelID: kernelID})
		},
		operation.DetailsFn(func(_ *operation.Bindings) any {
			return outcome.ProgramDetails{KernelID: kernelID}
		}),
	))

	nodes = append(nodes, operation.Try(
		"fs-remove-program", outcome.StepKindFSRemoveProgram, programName,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.RemoveProgramDir{KernelID: kernelID})
		},
	))

	return operation.Build(nodes...)
}
