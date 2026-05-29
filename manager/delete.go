package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// DeleteProgramsOpts configures program deletion.
type DeleteProgramsOpts struct {
	Recursive bool
	All       bool
}

// DeleteProgramResult records the outcome for one requested program.
type DeleteProgramResult struct {
	ProgramID kernel.ProgramID
	Err       error
}

// DeleteLinksOpts configures link deletion.
type DeleteLinksOpts struct {
	Recursive bool
}

// DeleteLinkResult records the outcome for one requested link.
type DeleteLinkResult struct {
	LinkID kernel.LinkID
	Err    error
}

// ResolveDeleteProgramIDs resolves the user-facing delete target into
// concrete program IDs. When all is true, every managed program is selected.
func (m *Manager) ResolveDeleteProgramIDs(ctx context.Context, all bool, explicit []kernel.ProgramID) ([]kernel.ProgramID, error) {
	if !all {
		return append([]kernel.ProgramID(nil), explicit...), nil
	}

	result, err := m.ListPrograms(ctx)
	if err != nil {
		return nil, fmt.Errorf("list programs: %w", err)
	}
	ids := make([]kernel.ProgramID, len(result.Programs))
	for i, prog := range result.Programs {
		ids[i] = prog.Record.ProgramID
	}
	return ids, nil
}

// DeletePrograms detaches each target program's links and unloads the
// program. With Recursive, dependants that share the target's maps are
// deleted before the target.
func (m *Manager) DeletePrograms(ctx context.Context, writeLock lock.WriterScope, ids []kernel.ProgramID, opts DeleteProgramsOpts) []DeleteProgramResult {
	results := make([]DeleteProgramResult, 0, len(ids))
	removed := make(map[kernel.ProgramID]bool, len(ids))
	for _, id := range ids {
		if removed[id] {
			results = append(results, DeleteProgramResult{ProgramID: id})
			continue
		}
		deleted, err := m.deleteProgram(ctx, writeLock, id, opts.Recursive)
		for _, deletedID := range deleted {
			removed[deletedID] = true
		}
		results = append(results, DeleteProgramResult{ProgramID: id, Err: err})
	}
	return results
}

// DeleteLinks detaches each link and unloads the owning program if the
// detach leaves it without remaining links. With Recursive, map-owner
// dependants of the orphaned program are deleted first.
func (m *Manager) DeleteLinks(ctx context.Context, writeLock lock.WriterScope, ids []kernel.LinkID, opts DeleteLinksOpts) []DeleteLinkResult {
	results := make([]DeleteLinkResult, 0, len(ids))
	for _, id := range ids {
		err := m.deleteLink(ctx, writeLock, id, opts.Recursive)
		results = append(results, DeleteLinkResult{LinkID: id, Err: err})
	}
	return results
}

func (m *Manager) deleteLink(ctx context.Context, writeLock lock.WriterScope, linkID kernel.LinkID, recursive bool) error {
	link, err := m.GetLink(ctx, linkID)
	if err != nil {
		return fmt.Errorf("get link: %w", err)
	}

	programID := link.ProgramID

	if err := m.Detach(ctx, writeLock, linkID); err != nil {
		return fmt.Errorf("detach: %w", err)
	}

	links, err := m.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links for program %d: %w", programID, err)
	}

	if len(links) == 0 {
		if _, err := m.deleteProgram(ctx, writeLock, programID, recursive); err != nil {
			return fmt.Errorf("delete orphaned program %d: %w", programID, err)
		}
	}

	return nil
}

func (m *Manager) deleteProgram(ctx context.Context, writeLock lock.WriterScope, programID kernel.ProgramID, recursive bool) ([]kernel.ProgramID, error) {
	var deleted []kernel.ProgramID
	if recursive {
		dependents, err := m.deleteDependents(ctx, writeLock, programID)
		deleted = append(deleted, dependents...)
		if err != nil {
			return deleted, err
		}
	}

	links, err := m.ListLinksByProgram(ctx, programID)
	if err != nil {
		return deleted, fmt.Errorf("list links: %w", err)
	}

	for _, link := range links {
		if err := m.Detach(ctx, writeLock, link.ID); err != nil {
			return deleted, fmt.Errorf("detach link %d: %w", link.ID, err)
		}
	}

	if err := m.Unload(ctx, writeLock, programID); err != nil {
		return deleted, fmt.Errorf("unload: %w", err)
	}

	return append(deleted, programID), nil
}

func (m *Manager) deleteDependents(ctx context.Context, writeLock lock.WriterScope, ownerID kernel.ProgramID) ([]kernel.ProgramID, error) {
	result, err := m.ListPrograms(ctx)
	if err != nil {
		return nil, fmt.Errorf("list programs: %w", err)
	}

	var deleted []kernel.ProgramID
	for _, prog := range result.Programs {
		if prog.Record.Handles.MapOwnerID != nil && *prog.Record.Handles.MapOwnerID == ownerID {
			dependents, err := m.deleteProgram(ctx, writeLock, prog.Record.ProgramID, true)
			deleted = append(deleted, dependents...)
			if err != nil {
				return deleted, fmt.Errorf("delete dependent program %d: %w", prog.Record.ProgramID, err)
			}
		}
	}

	return deleted, nil
}
