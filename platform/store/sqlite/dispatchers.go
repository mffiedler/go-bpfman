package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// ----------------------------------------------------------------------------
// Dispatcher Store Operations
// ----------------------------------------------------------------------------

// GetDispatcher retrieves a dispatcher by type, nsid, and ifindex.
func (s *sqliteStore) GetDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (dispatcher.State, error) {
	start := time.Now()
	row := s.stmtGetDispatcher.QueryRowContext(ctx, dispType, nsid, ifindex)

	var state dispatcher.State
	var dispTypeStr string
	err := row.Scan(&dispTypeStr, &state.Nsid, &state.Ifindex, &state.Revision,
		&state.KernelID, &state.LinkID, &state.Priority)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return dispatcher.State{}, fmt.Errorf("dispatcher (%s, %d, %d): %w", dispType, nsid, ifindex, platform.ErrRecordNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "error", err)
		return dispatcher.State{}, err
	}
	s.logger.Debug("sql", "stmt", "GetDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows", 1)

	state.Type = dispatcher.DispatcherType(dispTypeStr)
	return state, nil
}

// ListDispatchers returns all dispatchers. The returned slice has no guaranteed
// order; sorting for deterministic output is done in inspect.Snapshot.
func (s *sqliteStore) ListDispatchers(ctx context.Context) ([]dispatcher.State, error) {
	start := time.Now()
	rows, err := s.stmtListDispatchers.QueryContext(ctx)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListDispatchers", "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	var result []dispatcher.State
	for rows.Next() {
		var state dispatcher.State
		var dispTypeStr string
		if err := rows.Scan(&dispTypeStr, &state.Nsid, &state.Ifindex, &state.Revision,
			&state.KernelID, &state.LinkID, &state.Priority); err != nil {
			s.logger.Debug("sql", "stmt", "ListDispatchers", "duration_ms", msec(time.Since(start)), "error", err)
			return nil, err
		}
		state.Type = dispatcher.DispatcherType(dispTypeStr)
		result = append(result, state)
	}
	if err := rows.Err(); err != nil {
		s.logger.Debug("sql", "stmt", "ListDispatchers", "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}

	s.logger.Debug("sql", "stmt", "ListDispatchers", "duration_ms", msec(time.Since(start)), "rows", len(result))
	return result, nil
}

// SaveDispatcher creates or updates a dispatcher.
func (s *sqliteStore) SaveDispatcher(ctx context.Context, state dispatcher.State) error {
	now := time.Now().UTC().Format(time.RFC3339)

	start := time.Now()
	result, err := s.stmtSaveDispatcher.ExecContext(ctx,
		string(state.Type), state.Nsid, state.Ifindex, state.Revision,
		state.KernelID, state.LinkID,
		state.Priority, now, now)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveDispatcher", "args", []any{state.Type, state.Nsid, state.Ifindex, state.Revision, state.KernelID, state.LinkID, state.Priority, "(timestamp)", "(timestamp)"}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("save dispatcher: %w", err)
	}
	rows, _ := result.RowsAffected()
	s.logger.Debug("sql", "stmt", "SaveDispatcher", "args", []any{state.Type, state.Nsid, state.Ifindex, state.Revision, state.KernelID, state.LinkID, state.Priority, "(timestamp)", "(timestamp)"}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)

	return nil
}

// DeleteDispatcher removes a dispatcher by type, nsid, and ifindex.
func (s *sqliteStore) DeleteDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) error {
	start := time.Now()
	result, err := s.stmtDeleteDispatcher.ExecContext(ctx, dispType, nsid, ifindex)
	if err != nil {
		s.logger.Debug("sql", "stmt", "DeleteDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("delete dispatcher: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	s.logger.Debug("sql", "stmt", "DeleteDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)
	if rows == 0 {
		return fmt.Errorf("dispatcher (%s, %d, %d): %w", dispType, nsid, ifindex, platform.ErrRecordNotFound)
	}

	return nil
}

// IncrementRevision atomically increments the dispatcher revision.
// Returns the new revision number. Wraps from MaxUint32 to 1.
// For atomicity with other operations, wrap in RunInTransaction.
func (s *sqliteStore) IncrementRevision(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (uint32, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Use CASE to handle wrap-around at MaxUint32
	start := time.Now()
	result, err := s.stmtIncrementRevision.ExecContext(ctx, now, dispType, nsid, ifindex)
	if err != nil {
		s.logger.Debug("sql", "stmt", "IncrementRevision", "args", []any{"(timestamp)", dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, fmt.Errorf("increment revision: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	s.logger.Debug("sql", "stmt", "IncrementRevision", "args", []any{"(timestamp)", dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)
	if rows == 0 {
		return 0, fmt.Errorf("dispatcher (%s, %d, %d): %w", dispType, nsid, ifindex, platform.ErrRecordNotFound)
	}

	// Fetch the new revision
	start = time.Now()
	var newRevision uint32
	err = s.stmtGetDispatcherByType.QueryRowContext(ctx, dispType, nsid, ifindex).Scan(&newRevision)
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetDispatcherByType", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, fmt.Errorf("fetch new revision: %w", err)
	}
	s.logger.Debug("sql", "stmt", "GetDispatcherByType", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows", 1)

	return newRevision, nil
}

// CountDispatcherLinks returns the number of extension links attached
// to the dispatcher identified by its kernel program ID.
func (s *sqliteStore) CountDispatcherLinks(ctx context.Context, dispatcherKernelID kernel.ProgramID) (int, error) {
	start := time.Now()
	var count int
	err := s.stmtCountDispatcherLinks.QueryRowContext(ctx, dispatcherKernelID, dispatcherKernelID).Scan(&count)
	if err != nil {
		s.logger.Debug("sql", "stmt", "CountDispatcherLinks", "args", []any{dispatcherKernelID}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, err
	}
	s.logger.Debug("sql", "stmt", "CountDispatcherLinks", "args", []any{dispatcherKernelID}, "duration_ms", msec(time.Since(start)), "count", count)
	return count, nil
}
