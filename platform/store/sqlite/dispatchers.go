package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
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
func (s *sqliteStore) GetDispatcher(ctx context.Context, dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) (dispatcher.State, error) {
	start := time.Now()
	row := s.stmtGetDispatcher.QueryRowContext(ctx, dispType.String(), nsid, ifindex)

	var state dispatcher.State
	var dispTypeStr string
	err := row.Scan(&dispTypeStr, &state.Nsid, &state.Ifindex, &state.Revision,
		&state.ProgramID, &state.LinkID, &state.Priority)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return dispatcher.State{}, fmt.Errorf("dispatcher (%s, %d, %d): %w", dispType, nsid, ifindex, platform.ErrRecordNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "error", err)
		return dispatcher.State{}, err
	}
	s.logger.Debug("sql", "stmt", "GetDispatcher", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows", 1)

	parsed, err := dispatcher.ParseDispatcherType(dispTypeStr)
	if err != nil {
		return dispatcher.State{}, fmt.Errorf("invalid dispatcher type in DB: %w", err)
	}
	state.Type = parsed
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
			&state.ProgramID, &state.LinkID, &state.Priority); err != nil {
			s.logger.Debug("sql", "stmt", "ListDispatchers", "duration_ms", msec(time.Since(start)), "error", err)
			return nil, err
		}
		parsed, err := dispatcher.ParseDispatcherType(dispTypeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid dispatcher type in DB: %w", err)
		}
		state.Type = parsed
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
		state.Type.String(), state.Nsid, state.Ifindex, state.Revision,
		state.ProgramID, state.LinkID,
		state.Priority, now, now)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveDispatcher", "args", []any{state.Type, state.Nsid, state.Ifindex, state.Revision, state.ProgramID, state.LinkID, state.Priority, "(timestamp)", "(timestamp)"}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("save dispatcher: %w", err)
	}
	rows, _ := result.RowsAffected()
	s.logger.Debug("sql", "stmt", "SaveDispatcher", "args", []any{state.Type, state.Nsid, state.Ifindex, state.Revision, state.ProgramID, state.LinkID, state.Priority, "(timestamp)", "(timestamp)"}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)

	return nil
}

// DeleteDispatcher removes a dispatcher by type, nsid, and ifindex.
func (s *sqliteStore) DeleteDispatcher(ctx context.Context, dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) error {
	start := time.Now()
	result, err := s.stmtDeleteDispatcher.ExecContext(ctx, dispType.String(), nsid, ifindex)
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
func (s *sqliteStore) IncrementRevision(ctx context.Context, dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) (uint32, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Use CASE to handle wrap-around at MaxUint32
	start := time.Now()
	result, err := s.stmtIncrementRevision.ExecContext(ctx, now, dispType.String(), nsid, ifindex)
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
	err = s.stmtGetDispatcherByType.QueryRowContext(ctx, dispType.String(), nsid, ifindex).Scan(&newRevision)
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetDispatcherByType", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, fmt.Errorf("fetch new revision: %w", err)
	}
	s.logger.Debug("sql", "stmt", "GetDispatcherByType", "args", []any{dispType, nsid, ifindex}, "duration_ms", msec(time.Since(start)), "rows", 1)

	return newRevision, nil
}

// CountDispatcherLinks returns the number of extension links attached
// to the dispatcher identified by its program ID.
func (s *sqliteStore) CountDispatcherLinks(ctx context.Context, dispatcherProgramID kernel.ProgramID) (int, error) {
	start := time.Now()
	var count int
	err := s.stmtCountDispatcherLinks.QueryRowContext(ctx, dispatcherProgramID, dispatcherProgramID).Scan(&count)
	if err != nil {
		s.logger.Debug("sql", "stmt", "CountDispatcherLinks", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, err
	}
	s.logger.Debug("sql", "stmt", "CountDispatcherLinks", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "count", count)
	return count, nil
}

// ListDispatcherSlots returns occupied extension slots for a
// dispatcher, including position, priority, and program name. Results
// are ordered by (priority ASC, program_name ASC).
func (s *sqliteStore) ListDispatcherSlots(ctx context.Context, dispatcherProgramID kernel.ProgramID) ([]platform.DispatcherSlot, error) {
	start := time.Now()
	rows, err := s.stmtListDispatcherSlots.QueryContext(ctx, dispatcherProgramID, dispatcherProgramID)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListDispatcherSlots", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	var result []platform.DispatcherSlot
	for rows.Next() {
		var slot platform.DispatcherSlot
		var proceedOnJSON string
		var progPinPath string
		var linkID int64
		var programID int64
		var ifname string
		if err := rows.Scan(&slot.Position, &slot.Priority, &slot.ProgramName, &proceedOnJSON,
			&progPinPath, &linkID, &programID, &ifname); err != nil {
			s.logger.Debug("sql", "stmt", "ListDispatcherSlots", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
			return nil, err
		}
		slot.ProgPinPath = progPinPath
		slot.LinkID = kernel.LinkID(linkID)
		slot.ProgramID = kernel.ProgramID(programID)
		slot.Ifname = ifname
		// proceed_on is stored as a JSON array of action codes
		// (e.g., [2,31] for XDP_PASS and XDP_DISPATCHER_RETURN).
		// Reconstruct the bitmask: bit v is set for each code v.
		var actions []int32
		if err := json.Unmarshal([]byte(proceedOnJSON), &actions); err != nil {
			s.logger.Debug("sql", "stmt", "ListDispatcherSlots", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
			return nil, fmt.Errorf("unmarshal proceed_on: %w", err)
		}
		var bitmask uint32
		for _, v := range actions {
			if v >= 0 && v < 32 {
				bitmask |= 1 << uint(v)
			}
		}
		slot.ProceedOn = bitmask
		result = append(result, slot)
	}
	if err := rows.Err(); err != nil {
		s.logger.Debug("sql", "stmt", "ListDispatcherSlots", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}

	s.logger.Debug("sql", "stmt", "ListDispatcherSlots", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "rows", len(result))
	return result, nil
}

// DeleteDispatcherLinkDetails deletes all link detail records for a
// dispatcher. This removes entries from both link_xdp_details and
// link_tc_details where dispatcher_program_id matches. The parent
// links table entries are not affected.
func (s *sqliteStore) DeleteDispatcherLinkDetails(ctx context.Context, dispatcherProgramID kernel.ProgramID) error {
	start := time.Now()

	if _, err := s.stmtDeleteXDPDispatcherLinkDetails.ExecContext(ctx, dispatcherProgramID); err != nil {
		s.logger.Debug("sql", "stmt", "DeleteXDPDispatcherLinkDetails", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("delete XDP dispatcher link details: %w", err)
	}

	if _, err := s.stmtDeleteTCDispatcherLinkDetails.ExecContext(ctx, dispatcherProgramID); err != nil {
		s.logger.Debug("sql", "stmt", "DeleteTCDispatcherLinkDetails", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("delete TC dispatcher link details: %w", err)
	}

	s.logger.Debug("sql", "stmt", "DeleteDispatcherLinkDetails", "args", []any{dispatcherProgramID}, "duration_ms", msec(time.Since(start)))
	return nil
}
