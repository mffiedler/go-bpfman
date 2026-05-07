package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/platform"
)

// unload removes a program's kernel-side artefacts and, when persisted
// is true, deletes the program record from the store.
//
// This is the internal workhorse; it takes data directly, bypassing
// the store lookup and dependency checks that the public Unload
// performs.
//
// Failure contract for destructive teardown:
//
//  1. Kernel-side detach (per-link DetachLink, then unloading the
//     program pin) is the point of no return. If any of these fail
//     the program may still be live, or partially live for multi-link
//     programs; the function aborts and surfaces the error so the
//     caller can retry or hand off to repair. detachAllLinks is
//     fail-fast: a partial detach leaves the remaining links for
//     coherency to finish, which is preferable to attempting further
//     destructive work behind a failure.
//
//  2. Once the program is unloaded the user-visible state is "the
//     program is gone". The post-detach contract is therefore
//     log-only with one exception: deleteProgramRecord. A phantom
//     row is the worst residue class, and unlike the other
//     failures it self-heals on retry (the row is still present,
//     getProgram succeeds, the whole sequence re-runs cleanly).
//     Every other post-detach step --- map pin removal, shared-map
//     bookkeeping, the per-program links and bytecode directories,
//     and the empty-dispatcher cleanup --- is warned and discarded.
//     Joining their failures into the returned error only produced
//     ErrProgramNotFound on the caller's retry, which is a false
//     negative: the program really is gone, the residue is
//     internal and not actionable from outside. Coherency, doctor,
//     and GC repair the residue.
//
// This mirrors the contract on removeEmptyDispatcher. The previous
// plan-based version inherited operation.Run0's stop-on-first-error
// semantics, which after the program-unload point of no return left
// store rows referencing kernel attachments that no longer existed
// on transient bpffs failure --- the worst of both worlds. Naming
// the steps and stating the contract here makes the intended
// semantics reviewable and brings program unload into line with the
// dispatcher teardown lifecycle.
func (m *Manager) unload(ctx context.Context, programID kernel.ProgramID, programName string, links []bpfman.LinkRecord, persisted bool) error {
	progPinPath := m.rt.BPFFS().ProgPinPath(programID)
	mapsDir := m.rt.BPFFS().MapPinDir(programID)
	linksDir := m.rt.BPFFS().LinkPinDir(programID)

	m.logger.DebugContext(ctx, "unloading program",
		"program_id", programID,
		"name", programName,
		"links", len(links),
		"persisted", persisted)

	// Point of no return.
	if err := m.detachAllLinks(ctx, links); err != nil {
		return fmt.Errorf("detach links for program %d: %w", programID, err)
	}
	if err := m.unloadKernelProgram(ctx, progPinPath); err != nil {
		return fmt.Errorf("unload program %d: %w", programID, err)
	}

	// Post-detach cleanup. Each step is independent; later steps run
	// even if earlier steps fail. Only deleteProgramRecord joins into
	// the returned error (see the failure contract above): every
	// other step is warned and left for coherency/doctor/GC, so a
	// transient bpffs or store-cleanup failure cannot turn a
	// completed unload into a false-negative on the caller's retry.
	var errs []error
	if err := m.removeProgramLinksDir(linksDir); err != nil {
		m.logger.WarnContext(ctx, "failed to remove orphaned links directory",
			"program_id", programID,
			"path", linksDir,
			"error", err)
	}
	if err := m.removeProgramMapsPins(ctx, mapsDir); err != nil {
		m.logger.WarnContext(ctx, "failed to remove orphaned map pins",
			"program_id", programID,
			"path", mapsDir,
			"error", err)
	}
	if err := m.cleanupSharedMapPins(ctx, programID); err != nil {
		m.logger.WarnContext(ctx, "failed to cleanup shared map pins",
			"program_id", programID,
			"error", err)
	}
	if persisted {
		if err := m.deleteProgramRecord(ctx, programID); err != nil {
			errs = append(errs, fmt.Errorf("delete program record: %w", err))
		}
	}
	if err := m.removeProgramBytecodeDir(programID); err != nil {
		m.logger.WarnContext(ctx, "failed to remove orphaned bytecode directory",
			"program_id", programID,
			"path", m.rt.Bytecode().ProgramDir(programID),
			"error", err)
	}
	// Final best-effort step: clean up any dispatcher left empty by
	// the link removal above. Must come after deleteProgramRecord
	// because the SQL ON DELETE CASCADE on links.kernel_prog_id is
	// what makes the dispatcher observably empty. cleanupEmptyDispatchers
	// returns no error: per-dispatcher failures are logged inside it
	// and repaired by coherency/doctor/GC. If deleteProgramRecord
	// itself failed, the link rows remain, the dispatcher is
	// observed non-empty, and this call is a no-op --- correct under
	// the documented contract.
	m.cleanupEmptyDispatchers(ctx, collectDispatcherKeys(links))
	return errors.Join(errs...)
}

// detachAllLinks performs BPF_LINK_DETACH on each persisted link in
// turn. It is the first half of the kernel-side point of no return:
// once any link detach has succeeded the program's attachment state
// has been mutated, and a clean inverse no longer exists. The
// function is fail-fast --- if a detach fails the remaining links
// are left for coherency or a retry to clean up rather than pressed
// through additional destructive work.
//
// Ephemeral links (PinPath nil) are not represented in bpffs and
// require no kernel-side detach; the in-memory link object will be
// dropped when its program is unloaded.
func (m *Manager) detachAllLinks(ctx context.Context, links []bpfman.LinkRecord) error {
	for _, link := range links {
		if link.PinPath == nil {
			continue
		}
		if err := m.kernel.DetachLink(ctx, *link.PinPath); err != nil {
			return fmt.Errorf("detach link %d: %w", link.ID, err)
		}
	}
	return nil
}

// unloadKernelProgram drops the bpffs program pin. Once this returns
// successfully the program has no userland pin holding it alive; the
// kernel reclaims it once the refcount reaches zero. This is the
// second half of the kernel-side point of no return.
func (m *Manager) unloadKernelProgram(ctx context.Context, progPinPath bpfman.ProgPinPath) error {
	return m.kernel.Unload(ctx, progPinPath.String())
}

// removeProgramLinksDir removes the per-program links pin directory.
// The directory is empty by the time this runs because detachAllLinks
// has unpinned each link; removing the directory is filesystem
// hygiene that lets bpffs reflect the program's absence.
func (m *Manager) removeProgramLinksDir(linksDir bpfman.LinkDir) error {
	return m.rt.BPFFS().RemoveLinkDir(linksDir)
}

// removeProgramMapsPins removes the program's map pins. After the
// program is unloaded the maps either belong to other programs
// (refcounted, handled separately) or are now unreferenced and the
// kernel will reclaim them once the userland pins are dropped.
func (m *Manager) removeProgramMapsPins(ctx context.Context, mapsDir bpfman.MapDir) error {
	return m.kernel.Unload(ctx, mapsDir.String())
}

// cleanupSharedMapPins decrements the program's references in the
// shared-map pin table and removes any pins that no other program
// still references. Per-pin removal failures are warned, not
// returned, because the store has already been updated and the bpffs
// orphan is repairable by GC; what matters at this layer is that the
// store row no longer claims the pin.
func (m *Manager) cleanupSharedMapPins(ctx context.Context, programID kernel.ProgramID) error {
	var orphaned []string
	if err := m.store.RunInTransaction(ctx, func(tx platform.Store) error {
		var txErr error
		orphaned, txErr = tx.DeleteSharedMapPins(ctx, programID)
		return txErr
	}); err != nil {
		return err
	}
	bpffs := m.rt.BPFFS()
	for _, mapName := range orphaned {
		path := bpffs.SharedMapPin(mapName)
		if err := bpffs.RemoveSharedMapPin(path); err != nil {
			m.logger.WarnContext(ctx, "failed to remove orphaned shared map pin",
				"program_id", programID,
				"path", path,
				"error", err)
		}
	}
	return nil
}

// deleteProgramRecord removes the program row from the store. It is
// the last destructive step: by the time it runs, the kernel
// attachment is gone and bpffs cleanup has been attempted, so a
// failure here only leaves a stale row that coherency can repair.
func (m *Manager) deleteProgramRecord(ctx context.Context, programID kernel.ProgramID) error {
	return m.store.Delete(ctx, programID)
}

// removeProgramBytecodeDir removes the program's bytecode staging
// directory. This is filesystem hygiene only; the kernel never
// referenced this directory after Load completed.
func (m *Manager) removeProgramBytecodeDir(programID kernel.ProgramID) error {
	return m.rt.Bytecode().RemoveProgramDir(m.rt.Bytecode().ProgramDir(programID))
}

// Unload removes a BPF program, its links, and metadata.
//
// Preflight failures (store lookup, dependency check) return plain
// errors. The post-detach contract documented on m.unload means a
// successful Unload followed by a paranoid retry never sees a
// false-negative residue error: only the kernel-side detach and
// deleteProgramRecord can produce a returned error, and both retry
// cleanly. ErrProgramNotFound from preflight is informative ("you
// asked to unload an ID that does not exist") and is left in place.
func (m *Manager) Unload(ctx context.Context, writeLock lock.WriterScope, programID kernel.ProgramID) error {
	ctx, err := m.gcOnEntry(ctx, writeLock)
	if err != nil {
		return err
	}

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

	m.logger.InfoContext(ctx, "unloading program", "program_id", programID, "links", len(links))

	if err := m.unload(ctx, programID, programName, links, true); err != nil {
		return err
	}

	m.logger.InfoContext(ctx, "unloaded program", "program_id", programID)
	return nil
}
