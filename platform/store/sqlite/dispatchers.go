package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// ----------------------------------------------------------------------------
// Dispatcher Store Operations
// ----------------------------------------------------------------------------

// dispatcherDirection returns the TC direction string for a
// dispatcher type. Returns "" for XDP.
func dispatcherDirection(dt dispatcher.DispatcherType) string {
	switch dt {
	case dispatcher.DispatcherTypeTCIngress:
		return "ingress"
	case dispatcher.DispatcherTypeTCEgress:
		return "egress"
	default:
		return ""
	}
}

// scanDispatcherRuntime scans the dispatcher row fields (program_id,
// link_id, priority) into a DispatcherRuntime, handling nullable
// link_id and priority.
func scanDispatcherRuntime(programID kernel.ProgramID, nullLinkID sql.NullInt64, priority int) platform.DispatcherRuntime {
	rt := platform.DispatcherRuntime{ProgramID: programID}
	if nullLinkID.Valid {
		lid := kernel.LinkID(nullLinkID.Int64)
		rt.LinkID = &lid
	}
	if priority > 0 {
		p := uint16(priority)
		rt.FilterPriority = &p
	}
	return rt
}

// GetDispatcherSnapshot retrieves a complete snapshot of a dispatcher
// and all its extension members. Uses raw SQL via s.conn so it
// automatically participates in RunInTransaction.
func (s *sqliteStore) GetDispatcherSnapshot(ctx context.Context, key dispatcher.Key) (platform.DispatcherSnapshot, error) {
	start := time.Now()

	// Fetch dispatcher row.
	var revision uint32
	var programID kernel.ProgramID
	var nullLinkID sql.NullInt64
	var priority int

	err := s.conn.QueryRowContext(ctx,
		`SELECT revision, program_id, link_id, priority
		 FROM dispatchers WHERE type = ? AND nsid = ? AND ifindex = ?`,
		key.Type.String(), key.Nsid, key.Ifindex,
	).Scan(&revision, &programID, &nullLinkID, &priority)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetDispatcherSnapshot", "args", []any{key}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return platform.DispatcherSnapshot{}, fmt.Errorf("dispatcher (%s, %d, %d): %w", key.Type, key.Nsid, key.Ifindex, platform.ErrRecordNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetDispatcherSnapshot", "args", []any{key}, "duration_ms", msec(time.Since(start)), "error", err)
		return platform.DispatcherSnapshot{}, fmt.Errorf("get dispatcher snapshot: %w", err)
	}

	snap := platform.DispatcherSnapshot{
		Key:      key,
		Revision: revision,
		Runtime:  scanDispatcherRuntime(programID, nullLinkID, priority),
	}

	// Fetch members from the appropriate detail table.
	var memberQuery string
	var queryArgs []any

	if key.Type == dispatcher.DispatcherTypeXDP {
		memberQuery = `
			SELECT d.position, d.priority, p.program_name, d.proceed_on,
			       p.pin_path, l.link_id, l.kernel_prog_id, l.pin_path, d.interface
			FROM link_xdp_details d
			JOIN links l ON d.link_id = l.link_id
			JOIN managed_programs p ON l.kernel_prog_id = p.program_id
			WHERE d.nsid = ? AND d.ifindex = ?
			ORDER BY d.priority ASC, p.program_name ASC`
		queryArgs = []any{key.Nsid, key.Ifindex}
	} else {
		dir := dispatcherDirection(key.Type)
		memberQuery = `
			SELECT d.position, d.priority, p.program_name, d.proceed_on,
			       p.pin_path, l.link_id, l.kernel_prog_id, l.pin_path, d.interface
			FROM link_tc_details d
			JOIN links l ON d.link_id = l.link_id
			JOIN managed_programs p ON l.kernel_prog_id = p.program_id
			WHERE d.nsid = ? AND d.ifindex = ? AND d.direction = ?
			ORDER BY d.priority ASC, p.program_name ASC`
		queryArgs = []any{key.Nsid, key.Ifindex, dir}
	}

	rows, err := s.conn.QueryContext(ctx, memberQuery, queryArgs...)
	if err != nil {
		return platform.DispatcherSnapshot{}, fmt.Errorf("get dispatcher snapshot members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m platform.DispatcherMember
		var proceedOnJSON string
		var linkPinPath sql.NullString
		if err := rows.Scan(&m.Position, &m.Priority, &m.ProgramName, &proceedOnJSON,
			&m.ProgPinPath, &m.LinkID, &m.ProgramID, &linkPinPath, &m.Ifname); err != nil {
			return platform.DispatcherSnapshot{}, fmt.Errorf("scan dispatcher member: %w", err)
		}
		if linkPinPath.Valid {
			m.LinkPinPath = linkPinPath.String
		}

		var actions []int32
		if err := json.Unmarshal([]byte(proceedOnJSON), &actions); err != nil {
			return platform.DispatcherSnapshot{}, fmt.Errorf("unmarshal proceed_on: %w", err)
		}
		var bitmask uint32
		for _, v := range actions {
			if v >= 0 && v < 32 {
				bitmask |= 1 << uint(v)
			}
		}
		m.ProceedOn = bitmask

		snap.Members = append(snap.Members, m)
	}
	if err := rows.Err(); err != nil {
		return platform.DispatcherSnapshot{}, fmt.Errorf("iterate dispatcher members: %w", err)
	}

	s.logger.Debug("sql", "stmt", "GetDispatcherSnapshot", "args", []any{key}, "duration_ms", msec(time.Since(start)), "members", len(snap.Members))
	return snap, nil
}

// ListDispatcherSummaries returns lightweight summaries of all
// dispatchers with member counts. Uses a correlated subquery to
// count members without joining detail tables.
func (s *sqliteStore) ListDispatcherSummaries(ctx context.Context) ([]platform.DispatcherSummary, error) {
	start := time.Now()

	rows, err := s.conn.QueryContext(ctx, `
		SELECT d.type, d.nsid, d.ifindex, d.revision, d.program_id, d.link_id,
		       d.priority,
		    (SELECT COUNT(*) FROM link_xdp_details x
		     WHERE x.nsid = d.nsid AND x.ifindex = d.ifindex
		       AND d.type = 'xdp') +
		    (SELECT COUNT(*) FROM link_tc_details t
		     WHERE t.nsid = d.nsid AND t.ifindex = d.ifindex
		       AND t.direction = CASE d.type
		           WHEN 'tc-ingress' THEN 'ingress'
		           WHEN 'tc-egress' THEN 'egress'
		           ELSE '' END) AS member_count
		FROM dispatchers d`)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListDispatcherSummaries", "duration_ms", msec(time.Since(start)), "error", err)
		return nil, fmt.Errorf("list dispatcher summaries: %w", err)
	}
	defer rows.Close()

	var result []platform.DispatcherSummary
	for rows.Next() {
		var dispTypeStr string
		var summary platform.DispatcherSummary
		var programID kernel.ProgramID
		var nullLinkID sql.NullInt64
		var priority int
		if err := rows.Scan(&dispTypeStr, &summary.Key.Nsid, &summary.Key.Ifindex,
			&summary.Revision, &programID, &nullLinkID, &priority, &summary.MemberCount); err != nil {
			s.logger.Debug("sql", "stmt", "ListDispatcherSummaries", "duration_ms", msec(time.Since(start)), "error", err)
			return nil, fmt.Errorf("scan dispatcher summary: %w", err)
		}
		parsed, err := dispatcher.ParseDispatcherType(dispTypeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid dispatcher type in DB: %w", err)
		}
		summary.Key.Type = parsed
		summary.Runtime = scanDispatcherRuntime(programID, nullLinkID, priority)
		result = append(result, summary)
	}
	if err := rows.Err(); err != nil {
		s.logger.Debug("sql", "stmt", "ListDispatcherSummaries", "duration_ms", msec(time.Since(start)), "error", err)
		return nil, fmt.Errorf("iterate dispatcher summaries: %w", err)
	}

	s.logger.Debug("sql", "stmt", "ListDispatcherSummaries", "duration_ms", msec(time.Since(start)), "rows", len(result))
	return result, nil
}

// ReplaceDispatcherSnapshot atomically replaces all persisted state
// for a dispatcher's attach point. Deletes old extension link records
// by attach point, upserts the dispatcher row, and inserts new
// member link records.
func (s *sqliteStore) ReplaceDispatcherSnapshot(ctx context.Context, snap platform.DispatcherSnapshot) error {
	start := time.Now()
	now := time.Now().UTC().Format(time.RFC3339)

	// Step 1: Delete old extension link base rows by attach point.
	// CASCADE from links -> detail tables removes the detail rows.
	if snap.Key.Type == dispatcher.DispatcherTypeXDP {
		if _, err := s.conn.ExecContext(ctx,
			`DELETE FROM links WHERE link_id IN
			    (SELECT link_id FROM link_xdp_details WHERE nsid = ? AND ifindex = ?)`,
			snap.Key.Nsid, snap.Key.Ifindex); err != nil {
			return fmt.Errorf("delete old XDP extension links: %w", err)
		}
	} else {
		dir := dispatcherDirection(snap.Key.Type)
		if _, err := s.conn.ExecContext(ctx,
			`DELETE FROM links WHERE link_id IN
			    (SELECT link_id FROM link_tc_details WHERE nsid = ? AND ifindex = ? AND direction = ?)`,
			snap.Key.Nsid, snap.Key.Ifindex, dir); err != nil {
			return fmt.Errorf("delete old TC extension links: %w", err)
		}
	}

	// Step 2: Upsert dispatcher row.
	var linkID any
	if snap.Runtime.LinkID != nil {
		linkID = *snap.Runtime.LinkID
	}
	var priority int
	if snap.Runtime.FilterPriority != nil {
		priority = int(*snap.Runtime.FilterPriority)
	}
	if _, err := s.conn.ExecContext(ctx,
		`INSERT INTO dispatchers (type, nsid, ifindex, revision, program_id, link_id, priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(type, nsid, ifindex) DO UPDATE SET
		   revision = excluded.revision,
		   program_id = excluded.program_id,
		   link_id = excluded.link_id,
		   priority = excluded.priority,
		   updated_at = excluded.updated_at`,
		snap.Key.Type.String(), snap.Key.Nsid, snap.Key.Ifindex,
		snap.Revision, snap.Runtime.ProgramID, linkID,
		priority, now, now); err != nil {
		return fmt.Errorf("upsert dispatcher: %w", err)
	}

	// Step 3: Insert base link row and detail row for each member.
	for _, m := range snap.Members {
		// Insert base link row.
		kind := "xdp"
		if snap.Key.Type != dispatcher.DispatcherTypeXDP {
			kind = "tc"
		}
		var pinPath any
		if m.LinkPinPath != "" {
			pinPath = m.LinkPinPath
		}
		isSynthetic := 0
		if bpfman.IsSyntheticLinkID(m.LinkID) {
			isSynthetic = 1
		}
		if _, err := s.conn.ExecContext(ctx,
			`INSERT INTO links (link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(link_id) DO UPDATE SET pin_path = excluded.pin_path`,
			m.LinkID, kind, m.ProgramID, pinPath, isSynthetic, now); err != nil {
			return fmt.Errorf("insert extension link %d: %w", m.LinkID, err)
		}

		// Insert detail row.
		proceedOnJSON, err := proceedOnToJSON(m.ProceedOn)
		if err != nil {
			return fmt.Errorf("marshal proceed_on for link %d: %w", m.LinkID, err)
		}

		if snap.Key.Type == dispatcher.DispatcherTypeXDP {
			if _, err := s.conn.ExecContext(ctx,
				`INSERT INTO link_xdp_details
				 (link_id, interface, ifindex, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision)
				 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
				m.LinkID, m.Ifname, snap.Key.Ifindex, m.Priority, m.Position,
				proceedOnJSON, snap.Key.Nsid, snap.Runtime.ProgramID, snap.Revision); err != nil {
				return fmt.Errorf("insert XDP detail for link %d: %w", m.LinkID, err)
			}
		} else {
			dir := dispatcherDirection(snap.Key.Type)
			if _, err := s.conn.ExecContext(ctx,
				`INSERT INTO link_tc_details
				 (link_id, interface, ifindex, direction, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision)
				 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
				m.LinkID, m.Ifname, snap.Key.Ifindex, dir, m.Priority, m.Position,
				proceedOnJSON, snap.Key.Nsid, snap.Runtime.ProgramID, snap.Revision); err != nil {
				return fmt.Errorf("insert TC detail for link %d: %w", m.LinkID, err)
			}
		}
	}

	s.logger.Debug("sql", "stmt", "ReplaceDispatcherSnapshot", "args", []any{snap.Key, snap.Revision},
		"duration_ms", msec(time.Since(start)), "members", len(snap.Members))
	return nil
}

// DeleteDispatcherSnapshot removes a dispatcher and all its extension
// link records by attach point key.
func (s *sqliteStore) DeleteDispatcherSnapshot(ctx context.Context, key dispatcher.Key) error {
	start := time.Now()

	// Step 1: Delete extension link base rows by attach point.
	// CASCADE removes the detail rows.
	if key.Type == dispatcher.DispatcherTypeXDP {
		if _, err := s.conn.ExecContext(ctx,
			`DELETE FROM links WHERE link_id IN
			    (SELECT link_id FROM link_xdp_details WHERE nsid = ? AND ifindex = ?)`,
			key.Nsid, key.Ifindex); err != nil {
			return fmt.Errorf("delete XDP extension links: %w", err)
		}
	} else {
		dir := dispatcherDirection(key.Type)
		if _, err := s.conn.ExecContext(ctx,
			`DELETE FROM links WHERE link_id IN
			    (SELECT link_id FROM link_tc_details WHERE nsid = ? AND ifindex = ? AND direction = ?)`,
			key.Nsid, key.Ifindex, dir); err != nil {
			return fmt.Errorf("delete TC extension links: %w", err)
		}
	}

	// Step 2: Delete dispatcher row.
	result, err := s.conn.ExecContext(ctx,
		`DELETE FROM dispatchers WHERE type = ? AND nsid = ? AND ifindex = ?`,
		key.Type.String(), key.Nsid, key.Ifindex)
	if err != nil {
		return fmt.Errorf("delete dispatcher: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		s.logger.Debug("sql", "stmt", "DeleteDispatcherSnapshot", "args", []any{key}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return fmt.Errorf("dispatcher (%s, %d, %d): %w", key.Type, key.Nsid, key.Ifindex, platform.ErrRecordNotFound)
	}

	s.logger.Debug("sql", "stmt", "DeleteDispatcherSnapshot", "args", []any{key}, "duration_ms", msec(time.Since(start)), "rows_affected", affected)
	return nil
}

// proceedOnToJSON converts a proceed-on bitmask to a JSON array of
// set bit positions, matching the storage format used by the schema.
func proceedOnToJSON(bitmask uint32) (string, error) {
	var actions []int32
	for i := 0; i < 32; i++ {
		if bitmask&(1<<uint(i)) != 0 {
			actions = append(actions, int32(i))
		}
	}
	if actions == nil {
		return "[]", nil
	}
	b, err := json.Marshal(actions)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
