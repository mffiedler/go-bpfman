package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// ----------------------------------------------------------------------------
// Link Registry Operations
// ----------------------------------------------------------------------------

// DeleteLink removes link metadata by link ID.
// Due to CASCADE, this also removes the corresponding detail table entry.
// Dispatcher-backed links (xdp, tc) cannot be deleted through this
// method; they must be removed via DispatcherStore snapshot operations.
func (s *sqliteStore) DeleteLink(ctx context.Context, linkID kernel.LinkID) error {
	start := time.Now()

	// Check if this is a dispatcher-backed link.
	var kind string
	err := s.stmtGetLinkRegistry.QueryRowContext(ctx, linkID).Scan(
		new(int64), &kind, new(int64), new(sql.NullString), new(int), new(string))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("link %d: %w", linkID, platform.ErrRecordNotFound)
	}
	if err != nil {
		return fmt.Errorf("check link kind: %w", err)
	}
	if kind == "xdp" || kind == "tc" {
		return fmt.Errorf("link %d is dispatcher-backed (%s): must be removed via DispatcherStore", linkID, kind)
	}

	result, err := s.stmtDeleteLink.ExecContext(ctx, linkID)
	if err != nil {
		s.logger.Debug("sql", "stmt", "DeleteLink", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to delete link: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	s.logger.Debug("sql", "stmt", "DeleteLink", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)
	if rows == 0 {
		return fmt.Errorf("link %d: %w", linkID, platform.ErrRecordNotFound)
	}

	return nil
}

// GetLink retrieves link metadata by link ID using two-phase lookup.
func (s *sqliteStore) GetLink(ctx context.Context, linkID kernel.LinkID) (bpfman.LinkRecord, error) {
	// Phase 1: Get summary from registry
	start := time.Now()
	row := s.stmtGetLinkRegistry.QueryRowContext(ctx, linkID)

	record, err := s.scanLinkRecord(row)
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetLinkRegistry", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.LinkRecord{}, err
	}
	s.logger.Debug("sql", "stmt", "GetLinkRegistry", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	// Phase 2: Get details based on link kind
	details, err := s.getLinkDetails(ctx, record.Kind, record.ID)
	if err != nil {
		return bpfman.LinkRecord{}, err
	}
	record.Details = details

	return record, nil
}

// ListLinks returns all links with their details populated. The returned slice
// has no guaranteed order; sorting for deterministic output is done in
// inspect.Snapshot.
func (s *sqliteStore) ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error) {
	start := time.Now()
	rows, err := s.stmtListLinks.QueryContext(ctx)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListLinks", "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	result, err := s.scanLinkRecords(rows)
	if err != nil {
		return nil, err
	}
	s.logger.Debug("sql", "stmt", "ListLinks", "duration_ms", msec(time.Since(start)), "rows", len(result))

	// Batch-fetch all details and populate links
	if err := s.populateLinkDetails(ctx, result); err != nil {
		return nil, err
	}

	return result, nil
}

// ListLinksByProgram returns all links for a given program ID with
// their details populated.
func (s *sqliteStore) ListLinksByProgram(ctx context.Context, programID kernel.ProgramID) ([]bpfman.LinkRecord, error) {
	start := time.Now()
	rows, err := s.stmtListLinksByProgram.QueryContext(ctx, programID)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListLinksByProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	result, err := s.scanLinkRecords(rows)
	if err != nil {
		return nil, err
	}
	s.logger.Debug("sql", "stmt", "ListLinksByProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "rows", len(result))

	// Batch-fetch all details and populate links
	if err := s.populateLinkDetails(ctx, result); err != nil {
		return nil, err
	}

	return result, nil
}

// ListTCXLinksByInterface returns all TCX links for a given interface/direction/namespace.
// Used for computing attach order based on priority.
func (s *sqliteStore) ListTCXLinksByInterface(ctx context.Context, nsid uint64, ifindex uint32, direction string) ([]bpfman.TCXLinkInfo, error) {
	start := time.Now()
	rows, err := s.stmtListTCXLinksByInterface.QueryContext(ctx, nsid, ifindex, direction)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListTCXLinksByInterface", "args", []any{nsid, ifindex, direction}, "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	result := make([]bpfman.TCXLinkInfo, 0)
	for rows.Next() {
		var info bpfman.TCXLinkInfo
		if err := rows.Scan(&info.KernelLinkID, &info.KernelProgramID, &info.Priority); err != nil {
			return nil, fmt.Errorf("scan TCX link info: %w", err)
		}
		result = append(result, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate TCX links: %w", err)
	}
	s.logger.Debug("sql", "stmt", "ListTCXLinksByInterface", "args", []any{nsid, ifindex, direction}, "duration_ms", msec(time.Since(start)), "rows", len(result))
	return result, nil
}

// ----------------------------------------------------------------------------
// Batch Detail Population
// ----------------------------------------------------------------------------

// batchPopulateDetails queries all rows from a detail table and
// populates the Details field of matching links. The scanRow closure
// handles type-specific column scanning and post-processing.
func (s *sqliteStore) batchPopulateDetails(
	ctx context.Context,
	stmt *sql.Stmt,
	label string,
	links []bpfman.LinkRecord,
	linkIndex map[kernel.LinkID]int,
	scanRow func(*sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error),
) error {
	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		return fmt.Errorf("batch fetch %s details: %w", label, err)
	}
	defer rows.Close()

	for rows.Next() {
		linkID, details, err := scanRow(rows)
		if err != nil {
			return fmt.Errorf("scan %s details: %w", label, err)
		}
		if idx, ok := linkIndex[linkID]; ok {
			links[idx].Details = details
		}
	}
	return rows.Err()
}

// populateLinkDetails batch-fetches details from all detail tables and
// populates the Details field of each link. This is O(8) queries regardless
// of N links, rather than O(N+1) for per-link fetching.
func (s *sqliteStore) populateLinkDetails(ctx context.Context, links []bpfman.LinkRecord) error {
	if len(links) == 0 {
		return nil
	}

	linkIndex := make(map[kernel.LinkID]int, len(links))
	for i := range links {
		linkIndex[links[i].ID] = i
	}

	type batchEntry struct {
		stmt    *sql.Stmt
		label   string
		scanRow func(*sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error)
	}

	batches := []batchEntry{
		{s.stmtListAllTracepointDetails, "tracepoint", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.TracepointDetails
			if err := rows.Scan(&linkID, &d.Group, &d.Name); err != nil {
				return 0, nil, err
			}
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllKprobeDetails, "kprobe", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.KprobeDetails
			var retprobe int
			if err := rows.Scan(&linkID, &d.FnName, &d.Offset, &retprobe); err != nil {
				return 0, nil, err
			}
			d.Retprobe = retprobe == 1
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllUprobeDetails, "uprobe", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.UprobeDetails
			var fnName sql.NullString
			var pid sql.NullInt64
			var retprobe int
			if err := rows.Scan(&linkID, &d.Target, &fnName, &d.Offset, &pid, &retprobe); err != nil {
				return 0, nil, err
			}
			if fnName.Valid {
				d.FnName = fnName.String
			}
			if pid.Valid {
				d.PID = int32(pid.Int64)
			}
			d.Retprobe = retprobe == 1
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllFentryDetails, "fentry", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.FentryDetails
			if err := rows.Scan(&linkID, &d.FnName); err != nil {
				return 0, nil, err
			}
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllFexitDetails, "fexit", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.FexitDetails
			if err := rows.Scan(&linkID, &d.FnName); err != nil {
				return 0, nil, err
			}
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllXDPDetails, "xdp", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.XDPDetails
			var proceedOnJSON string
			var netns sql.NullString
			if err := rows.Scan(&linkID, &d.Interface, &d.Ifindex, &d.Priority, &d.Position,
				&proceedOnJSON, &netns, &d.Nsid, &d.DispatcherID, &d.Revision); err != nil {
				return 0, nil, err
			}
			if err := json.Unmarshal([]byte(proceedOnJSON), &d.ProceedOn); err != nil {
				return 0, nil, fmt.Errorf("unmarshal xdp proceed_on: %w", err)
			}
			if netns.Valid {
				d.Netns = netns.String
			}
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllTCDetails, "tc", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.TCDetails
			var dirStr string
			var proceedOnJSON string
			var netns sql.NullString
			if err := rows.Scan(&linkID, &d.Interface, &d.Ifindex, &dirStr, &d.Priority, &d.Position,
				&proceedOnJSON, &netns, &d.Nsid, &d.DispatcherID, &d.Revision); err != nil {
				return 0, nil, err
			}
			dir, err := bpfman.ParseTCDirection(dirStr)
			if err != nil {
				return 0, nil, fmt.Errorf("invalid tc direction in DB for link %d: %w", linkID, err)
			}
			d.Direction = dir
			if err := json.Unmarshal([]byte(proceedOnJSON), &d.ProceedOn); err != nil {
				return 0, nil, fmt.Errorf("unmarshal tc proceed_on: %w", err)
			}
			if netns.Valid {
				d.Netns = netns.String
			}
			return kernel.LinkID(linkID), d, nil
		}},
		{s.stmtListAllTCXDetails, "tcx", func(rows *sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error) {
			var linkID int64
			var d bpfman.TCXDetails
			var dirStr string
			var netns sql.NullString
			var nsid sql.NullInt64
			if err := rows.Scan(&linkID, &d.Interface, &d.Ifindex, &dirStr, &d.Priority, &netns, &nsid, &d.Position); err != nil {
				return 0, nil, err
			}
			dir, err := bpfman.ParseTCDirection(dirStr)
			if err != nil {
				return 0, nil, fmt.Errorf("invalid tcx direction in DB for link %d: %w", linkID, err)
			}
			d.Direction = dir
			if netns.Valid {
				d.Netns = netns.String
			}
			if nsid.Valid {
				d.Nsid = uint64(nsid.Int64)
			}
			return kernel.LinkID(linkID), d, nil
		}},
	}

	for _, b := range batches {
		if err := s.batchPopulateDetails(ctx, b.stmt, b.label, links, linkIndex, b.scanRow); err != nil {
			return err
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// SaveLink - Unified Link Save Method
// ----------------------------------------------------------------------------

// SaveLink saves a link spec with its details.
// The spec.ID is used as the link ID (kernel-assigned for real BPF links,
// or bpfman-assigned synthetic ID for perf_event-based links).
// Dispatches to the appropriate detail table based on spec.Details.Kind().
func (s *sqliteStore) SaveLink(ctx context.Context, spec bpfman.LinkRecord) error {
	if err := s.insertLinkRegistry(ctx, spec); err != nil {
		return err
	}

	if spec.Details == nil {
		return nil
	}

	id := spec.ID
	switch d := spec.Details.(type) {
	case bpfman.TracepointDetails:
		return s.saveDetails(ctx, s.stmtSaveTracepointDetails, "Tracepoint", func() ([]any, error) {
			return []any{id, d.Group, d.Name}, nil
		})
	case bpfman.KprobeDetails:
		return s.saveDetails(ctx, s.stmtSaveKprobeDetails, "Kprobe", func() ([]any, error) {
			retprobe := 0
			if d.Retprobe {
				retprobe = 1
			}
			return []any{id, d.FnName, d.Offset, retprobe}, nil
		})
	case bpfman.UprobeDetails:
		return s.saveDetails(ctx, s.stmtSaveUprobeDetails, "Uprobe", func() ([]any, error) {
			retprobe := 0
			if d.Retprobe {
				retprobe = 1
			}
			return []any{id, d.Target, d.FnName, d.Offset, d.PID, retprobe}, nil
		})
	case bpfman.FentryDetails:
		return s.saveDetails(ctx, s.stmtSaveFentryDetails, "Fentry", func() ([]any, error) {
			return []any{id, d.FnName}, nil
		})
	case bpfman.FexitDetails:
		return s.saveDetails(ctx, s.stmtSaveFexitDetails, "Fexit", func() ([]any, error) {
			return []any{id, d.FnName}, nil
		})
	case bpfman.XDPDetails:
		return s.saveDetails(ctx, s.stmtSaveXDPDetails, "XDP", func() ([]any, error) {
			proceedOnJSON, err := json.Marshal(d.ProceedOn)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal proceed_on: %w", err)
			}
			return []any{id, d.Interface, d.Ifindex, d.Priority, d.Position,
				string(proceedOnJSON), d.Netns, d.Nsid, d.DispatcherID}, nil
		})
	case bpfman.TCDetails:
		return s.saveDetails(ctx, s.stmtSaveTCDetails, "TC", func() ([]any, error) {
			proceedOnJSON, err := json.Marshal(d.ProceedOn)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal proceed_on: %w", err)
			}
			return []any{id, d.Interface, d.Ifindex, d.Direction.String(), d.Priority, d.Position,
				string(proceedOnJSON), d.Netns, d.Nsid, d.DispatcherID}, nil
		})
	case bpfman.TCXDetails:
		return s.saveDetails(ctx, s.stmtSaveTCXDetails, "TCX", func() ([]any, error) {
			return []any{id, d.Interface, d.Ifindex, d.Direction.String(), d.Priority, d.Netns, d.Nsid}, nil
		})
	default:
		return fmt.Errorf("unknown link details type: %T", d)
	}
}

// saveDetails executes an insert into a link detail table. The
// prepareArgs closure handles any type-specific marshalling and
// returns the argument list for ExecContext.
func (s *sqliteStore) saveDetails(
	ctx context.Context,
	stmt *sql.Stmt,
	label string,
	prepareArgs func() ([]any, error),
) error {
	args, err := prepareArgs()
	if err != nil {
		return err
	}

	start := time.Now()
	_, err = stmt.ExecContext(ctx, args...)
	if err != nil {
		s.logger.Debug("sql", "stmt", "Save"+label+"Details", "args", args, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert %s details: %w", label, err)
	}
	s.logger.Debug("sql", "stmt", "Save"+label+"Details", "args", args, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

// ----------------------------------------------------------------------------
// Helper Functions
// ----------------------------------------------------------------------------

// insertLinkRegistry inserts a spec into the links table.
func (s *sqliteStore) insertLinkRegistry(ctx context.Context, spec bpfman.LinkRecord) error {
	start := time.Now()

	// Derive is_synthetic from the link ID
	isSynthetic := 0
	if bpfman.IsSyntheticLinkID(spec.ID) {
		isSynthetic = 1
	}

	// Convert pin to nullable string for DB storage
	var pinPath sql.NullString
	if spec.PinPath != nil {
		pinPath = sql.NullString{String: spec.PinPath.String(), Valid: true}
	}

	_, err := s.stmtInsertLinkRegistry.ExecContext(ctx,
		spec.ID, spec.Kind.String(), spec.ProgramID,
		pinPath, isSynthetic, spec.CreatedAt.Format(time.RFC3339))
	if err != nil {
		s.logger.Debug("sql", "stmt", "InsertLinkRegistry", "args", []any{spec.ID, spec.Kind, spec.ProgramID, pinPath, isSynthetic, "(timestamp)"}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert link: %w", err)
	}

	s.logger.Debug("sql", "stmt", "InsertLinkRegistry", "args", []any{spec.ID, spec.Kind, spec.ProgramID, pinPath, isSynthetic, "(timestamp)"}, "duration_ms", msec(time.Since(start)))
	return nil
}

// scanLinkRecord scans a single row into a LinkRecord (without details).
// Row format: link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
func (s *sqliteStore) scanLinkRecord(row *sql.Row) (bpfman.LinkRecord, error) {
	var record bpfman.LinkRecord
	var linkID int64
	var kindStr string
	var programID kernel.ProgramID
	var pinPath sql.NullString
	var isSynthetic int
	var createdAtStr string

	err := row.Scan(&linkID, &kindStr, &programID, &pinPath, &isSynthetic, &createdAtStr)
	if err == sql.ErrNoRows {
		return bpfman.LinkRecord{}, platform.ErrRecordNotFound
	}
	if err != nil {
		return bpfman.LinkRecord{}, err
	}

	record.ID = kernel.LinkID(linkID)
	kind, err := bpfman.ParseLinkKind(kindStr)
	if err != nil {
		return bpfman.LinkRecord{}, fmt.Errorf("invalid link kind in DB for link %d: %w", linkID, err)
	}
	record.Kind = kind
	record.ProgramID = programID
	if pinPath.Valid {
		pin := bpfman.LinkPath(pinPath.String)
		record.PinPath = &pin
	}
	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return bpfman.LinkRecord{}, fmt.Errorf("invalid created_at timestamp for link %d: %q: %w", linkID, createdAtStr, err)
	}
	record.CreatedAt = createdAt

	return record, nil
}

// scanLinkRecords scans multiple rows into a slice of LinkRecord (without details).
// Row format: link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
func (s *sqliteStore) scanLinkRecords(rows *sql.Rows) ([]bpfman.LinkRecord, error) {
	var result []bpfman.LinkRecord

	for rows.Next() {
		var linkID int64
		var kindStr string
		var programID kernel.ProgramID
		var pinPath sql.NullString
		var isSynthetic int
		var createdAtStr string

		err := rows.Scan(&linkID, &kindStr, &programID, &pinPath, &isSynthetic, &createdAtStr)
		if err != nil {
			return nil, err
		}

		kind, err := bpfman.ParseLinkKind(kindStr)
		if err != nil {
			return nil, fmt.Errorf("invalid link kind in DB for link %d: %w", linkID, err)
		}
		record := bpfman.LinkRecord{
			ID:        kernel.LinkID(linkID),
			Kind:      kind,
			ProgramID: programID,
		}
		if pinPath.Valid {
			pin := bpfman.LinkPath(pinPath.String)
			record.PinPath = &pin
		}
		createdAt, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("invalid created_at timestamp for link %d: %q: %w", linkID, createdAtStr, err)
		}
		record.CreatedAt = createdAt

		result = append(result, record)
	}

	return result, rows.Err()
}

// getDetailsFromRow queries a single row from a detail table and
// handles the three-branch error pattern (ErrNoRows / other / success)
// with logging. The scan closure handles type-specific column scanning
// and post-processing.
func (s *sqliteStore) getDetailsFromRow(
	ctx context.Context,
	stmt *sql.Stmt,
	label string,
	linkID kernel.LinkID,
	scan func(*sql.Row) (bpfman.LinkDetails, error),
) (bpfman.LinkDetails, error) {
	start := time.Now()
	row := stmt.QueryRowContext(ctx, linkID)
	details, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Debug("sql", "stmt", "Get"+label+"Details", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return nil, fmt.Errorf("%s details for %d: %w", label, linkID, platform.ErrRecordNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "Get"+label+"Details", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	s.logger.Debug("sql", "stmt", "Get"+label+"Details", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)
	return details, nil
}

// getLinkDetails retrieves the type-specific details for a link.
func (s *sqliteStore) getLinkDetails(ctx context.Context, kind bpfman.LinkKind, linkID kernel.LinkID) (bpfman.LinkDetails, error) {
	switch kind {
	case bpfman.LinkKindTracepoint:
		return s.getDetailsFromRow(ctx, s.stmtGetTracepointDetails, "tracepoint", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.TracepointDetails
			err := row.Scan(&d.Group, &d.Name)
			return d, err
		})
	case bpfman.LinkKindKprobe, bpfman.LinkKindKretprobe:
		return s.getDetailsFromRow(ctx, s.stmtGetKprobeDetails, "kprobe", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.KprobeDetails
			var retprobe int
			if err := row.Scan(&d.FnName, &d.Offset, &retprobe); err != nil {
				return d, err
			}
			d.Retprobe = retprobe == 1
			return d, nil
		})
	case bpfman.LinkKindUprobe, bpfman.LinkKindUretprobe:
		return s.getDetailsFromRow(ctx, s.stmtGetUprobeDetails, "uprobe", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.UprobeDetails
			var fnName sql.NullString
			var pid sql.NullInt64
			var retprobe int
			if err := row.Scan(&d.Target, &fnName, &d.Offset, &pid, &retprobe); err != nil {
				return d, err
			}
			if fnName.Valid {
				d.FnName = fnName.String
			}
			if pid.Valid {
				d.PID = int32(pid.Int64)
			}
			d.Retprobe = retprobe == 1
			return d, nil
		})
	case bpfman.LinkKindFentry:
		return s.getDetailsFromRow(ctx, s.stmtGetFentryDetails, "fentry", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.FentryDetails
			err := row.Scan(&d.FnName)
			return d, err
		})
	case bpfman.LinkKindFexit:
		return s.getDetailsFromRow(ctx, s.stmtGetFexitDetails, "fexit", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.FexitDetails
			err := row.Scan(&d.FnName)
			return d, err
		})
	case bpfman.LinkKindXDP:
		return s.getDetailsFromRow(ctx, s.stmtGetXDPDetails, "xdp", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.XDPDetails
			var proceedOnJSON string
			var netns sql.NullString
			if err := row.Scan(&d.Interface, &d.Ifindex, &d.Priority, &d.Position,
				&proceedOnJSON, &netns, &d.Nsid, &d.DispatcherID, &d.Revision); err != nil {
				return d, err
			}
			if err := json.Unmarshal([]byte(proceedOnJSON), &d.ProceedOn); err != nil {
				return d, fmt.Errorf("failed to unmarshal proceed_on: %w", err)
			}
			if netns.Valid {
				d.Netns = netns.String
			}
			return d, nil
		})
	case bpfman.LinkKindTC:
		return s.getDetailsFromRow(ctx, s.stmtGetTCDetails, "tc", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.TCDetails
			var dirStr string
			var proceedOnJSON string
			var netns sql.NullString
			if err := row.Scan(&d.Interface, &d.Ifindex, &dirStr, &d.Priority, &d.Position,
				&proceedOnJSON, &netns, &d.Nsid, &d.DispatcherID, &d.Revision); err != nil {
				return d, err
			}
			dir, err := bpfman.ParseTCDirection(dirStr)
			if err != nil {
				return d, fmt.Errorf("invalid tc direction in DB for link %d: %w", linkID, err)
			}
			d.Direction = dir
			if err := json.Unmarshal([]byte(proceedOnJSON), &d.ProceedOn); err != nil {
				return d, fmt.Errorf("failed to unmarshal proceed_on: %w", err)
			}
			if netns.Valid {
				d.Netns = netns.String
			}
			return d, nil
		})
	case bpfman.LinkKindTCX:
		return s.getDetailsFromRow(ctx, s.stmtGetTCXDetails, "tcx", linkID, func(row *sql.Row) (bpfman.LinkDetails, error) {
			var d bpfman.TCXDetails
			var dirStr string
			var netns sql.NullString
			var nsid sql.NullInt64
			if err := row.Scan(&d.Interface, &d.Ifindex, &dirStr, &d.Priority, &netns, &nsid, &d.Position); err != nil {
				return d, err
			}
			dir, err := bpfman.ParseTCDirection(dirStr)
			if err != nil {
				return d, fmt.Errorf("invalid tcx direction in DB for link %d: %w", linkID, err)
			}
			d.Direction = dir
			if netns.Valid {
				d.Netns = netns.String
			}
			if nsid.Valid {
				d.Nsid = uint64(nsid.Int64)
			}
			return d, nil
		})
	default:
		return nil, fmt.Errorf("unknown link kind: %s", kind)
	}
}
