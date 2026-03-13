package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
)

// unload removes a program's kernel-side artefacts. If persisted is
// true, the plan also deletes the program record from the store.
//
// This is the internal workhorse; it takes data directly, bypassing
// the store lookup and dependency checks that the public Unload
// performs.
func (m *Manager) unload(ctx context.Context, programID kernel.ProgramID, programName string, links []bpfman.LinkRecord, persisted bool) error {
	progPinPath := m.rt.BPFFS().ProgPinPath(programID)
	mapsDir := m.rt.BPFFS().MapPinDir(programID)
	linksDir := m.rt.BPFFS().LinkPinDir(programID)

	plan := m.unloadPlan(programID, programName, progPinPath, mapsDir, linksDir, links, persisted)
	return operation.Run0(ctx, m.logger, m.executor, plan)
}

// Unload removes a BPF program, its links, and metadata.
//
// Preflight failures (store lookup, dependency check) return plain
// errors. Execution failures return plain errors.
func (m *Manager) Unload(ctx context.Context, programID kernel.ProgramID) error {
	// FETCH: Get metadata and links (for link cleanup)
	progSpec, err := m.getProgram(ctx, programID)
	if err != nil {
		// Distinguish "not found" from "not managed" by checking kernel.
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			if _, kerr := m.kernel.GetProgramByID(ctx, programID); kerr == nil {
				return bpfman.ErrProgramNotManaged{ID: programID}
			}
		}
		return err
	}

	programName := progSpec.Meta.Name

	// FETCH: Check for dependent programs (map sharing)
	// Programs that share maps with this program must be unloaded first.
	depCount, err := m.store.CountDependentPrograms(ctx, programID)
	if err != nil {
		return fmt.Errorf("check dependent programs for %d: %w", programID, err)
	}
	if depCount > 0 {
		return fmt.Errorf("cannot unload program %d: %d dependent program(s) share its maps; unload dependents first", programID, depCount)
	}

	links, err := m.store.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links for program %d: %w", programID, err)
	}

	// FETCH: Collect dispatcher keys for any TC/XDP links before
	// the unload actions delete them from the store. We need these
	// to check whether the dispatchers are now empty afterwards.
	dispatcherKeys := collectDispatcherKeys(links)

	m.logger.InfoContext(ctx, "unloading program", "program_id", programID, "links", len(links))

	if err := m.unload(ctx, programID, programName, links, true); err != nil {
		return err
	}

	// Clean up any dispatchers left empty by the link removal.
	m.cleanupEmptyDispatchers(ctx, dispatcherKeys)

	m.logger.InfoContext(ctx, "unloaded program", "program_id", programID)
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
func (m *Manager) unloadPlan(programID kernel.ProgramID, programName, progPinPath, mapsDir, linksDir string, links []bpfman.LinkRecord, persisted bool) operation.Plan {
	var nodes []operation.Node

	for _, link := range links {
		if link.PinPath == nil {
			continue
		}
		pinPath := link.PinPath.String()
		nodes = append(nodes, operation.DoAction("detach-link", fmt.Sprintf("%d", link.ID), action.DetachLink{PinPath: pinPath}))
	}

	nodes = append(nodes,
		operation.DoAction("remove-links-dir", linksDir, action.RemovePin{Path: linksDir}),
		operation.DoAction("unload-prog", programName, action.UnloadProgram{PinPath: progPinPath}),
		operation.DoAction("unload-maps", programName, action.RemoveMapsPins{PinPath: mapsDir}),
		operation.TryAction("cleanup-shared-maps", programName, action.CleanupSharedMapPins{ProgramID: programID}),
	)

	if persisted {
		nodes = append(nodes, operation.DoAction("delete-program", programName, action.DeleteProgram{ProgramID: programID}))
	}

	nodes = append(nodes, operation.TryAction("fs-remove-program", programName, action.RemoveProgramDir{Path: m.rt.Bytecode().ProgramDir(programID)}))

	return operation.Build(nodes...)
}
