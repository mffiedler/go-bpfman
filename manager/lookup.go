package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/platform/store"
)

// getProgram fetches a program from the store, translating
// store.ErrNotFound into the domain error ErrProgramNotFound.
func (m *Manager) getProgram(ctx context.Context, id uint32) (bpfman.ProgramRecord, error) {
	rec, err := m.store.Get(ctx, id)
	if err == nil {
		return rec, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return rec, bpfman.ErrProgramNotFound{ID: id}
	}
	return rec, fmt.Errorf("get program %d: %w", id, err)
}

// getLink fetches a link from the store, translating
// store.ErrNotFound into the domain error ErrLinkNotFound.
func (m *Manager) getLink(ctx context.Context, id bpfman.LinkID) (bpfman.LinkRecord, error) {
	rec, err := m.store.GetLink(ctx, id)
	if err == nil {
		return rec, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return rec, bpfman.ErrLinkNotFound{LinkID: id}
	}
	return rec, fmt.Errorf("get link %d: %w", id, err)
}
