package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/interpreter/store"
)

// ----------------------------------------------------------------------------
// Link Registry Operations
// ----------------------------------------------------------------------------

// DeleteLink removes link metadata by link ID.
// Due to CASCADE, this also removes the corresponding detail table entry.
func (s *sqliteStore) DeleteLink(ctx context.Context, linkID bpfman.LinkID) error {
	start := time.Now()
	result, err := s.stmtDeleteLink.ExecContext(ctx, uint32(linkID))
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
		return fmt.Errorf("link %d: %w", linkID, store.ErrNotFound)
	}

	return nil
}

// GetLink retrieves link metadata by link ID using two-phase lookup.
func (s *sqliteStore) GetLink(ctx context.Context, linkID bpfman.LinkID) (bpfman.LinkSpec, error) {
	// Phase 1: Get summary from registry
	start := time.Now()
	row := s.stmtGetLinkRegistry.QueryRowContext(ctx, int64(linkID))

	record, err := s.scanLinkRecord(row)
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetLinkRegistry", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.LinkSpec{}, err
	}
	s.logger.Debug("sql", "stmt", "GetLinkRegistry", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	// Phase 2: Get details based on link kind
	details, err := s.getLinkDetails(ctx, record.Kind, record.ID)
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record.Details = details

	return record, nil
}

// ListLinks returns all links. The returned slice has no guaranteed order;
// sorting for deterministic output is done in inspect.Snapshot.
func (s *sqliteStore) ListLinks(ctx context.Context) ([]bpfman.LinkSpec, error) {
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
	return result, nil
}

// ListLinksByProgram returns all links for a given program kernel ID.
func (s *sqliteStore) ListLinksByProgram(ctx context.Context, programKernelID uint32) ([]bpfman.LinkSpec, error) {
	start := time.Now()
	rows, err := s.stmtListLinksByProgram.QueryContext(ctx, programKernelID)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListLinksByProgram", "args", []any{programKernelID}, "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	result, err := s.scanLinkRecords(rows)
	if err != nil {
		return nil, err
	}
	s.logger.Debug("sql", "stmt", "ListLinksByProgram", "args", []any{programKernelID}, "duration_ms", msec(time.Since(start)), "rows", len(result))
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
// SaveLink - Unified Link Save Method
// ----------------------------------------------------------------------------

// SaveLink saves a link spec with its details.
// The spec.ID is used as the link ID (kernel-assigned for real BPF links,
// or bpfman-assigned synthetic ID for perf_event-based links).
// Dispatches to the appropriate detail table based on spec.Details.Kind().
func (s *sqliteStore) SaveLink(ctx context.Context, spec bpfman.LinkSpec) error {
	if err := s.insertLinkRegistry(ctx, spec); err != nil {
		return err
	}

	if spec.Details == nil {
		return nil
	}

	switch details := spec.Details.(type) {
	case bpfman.TracepointDetails:
		return s.saveTracepointDetails(ctx, spec.ID, details)
	case bpfman.KprobeDetails:
		return s.saveKprobeDetails(ctx, spec.ID, details)
	case bpfman.UprobeDetails:
		return s.saveUprobeDetails(ctx, spec.ID, details)
	case bpfman.FentryDetails:
		return s.saveFentryDetails(ctx, spec.ID, details)
	case bpfman.FexitDetails:
		return s.saveFexitDetails(ctx, spec.ID, details)
	case bpfman.XDPDetails:
		return s.saveXDPDetails(ctx, spec.ID, details)
	case bpfman.TCDetails:
		return s.saveTCDetails(ctx, spec.ID, details)
	case bpfman.TCXDetails:
		return s.saveTCXDetails(ctx, spec.ID, details)
	default:
		return fmt.Errorf("unknown link details type: %T", details)
	}
}

// ----------------------------------------------------------------------------
// Type-Specific Detail Save Methods (internal)
// ----------------------------------------------------------------------------

func (s *sqliteStore) saveTracepointDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.TracepointDetails) error {
	start := time.Now()
	_, err := s.stmtSaveTracepointDetails.ExecContext(ctx,
		uint32(linkID), details.Group, details.Name)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveTracepointDetails", "args", []any{linkID, details.Group, details.Name}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert tracepoint details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveTracepointDetails", "args", []any{linkID, details.Group, details.Name}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveKprobeDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.KprobeDetails) error {
	retprobe := 0
	if details.Retprobe {
		retprobe = 1
	}

	start := time.Now()
	_, err := s.stmtSaveKprobeDetails.ExecContext(ctx,
		uint32(linkID), details.FnName, details.Offset, retprobe)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveKprobeDetails", "args", []any{linkID, details.FnName, details.Offset, retprobe}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert kprobe details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveKprobeDetails", "args", []any{linkID, details.FnName, details.Offset, retprobe}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveUprobeDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.UprobeDetails) error {
	retprobe := 0
	if details.Retprobe {
		retprobe = 1
	}

	start := time.Now()
	_, err := s.stmtSaveUprobeDetails.ExecContext(ctx,
		uint32(linkID), details.Target, details.FnName, details.Offset, details.PID, retprobe)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveUprobeDetails", "args", []any{linkID, details.Target, details.FnName, details.Offset, details.PID, retprobe}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert uprobe details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveUprobeDetails", "args", []any{linkID, details.Target, details.FnName, details.Offset, details.PID, retprobe}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveFentryDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.FentryDetails) error {
	start := time.Now()
	_, err := s.stmtSaveFentryDetails.ExecContext(ctx, uint32(linkID), details.FnName)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveFentryDetails", "args", []any{linkID, details.FnName}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert fentry details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveFentryDetails", "args", []any{linkID, details.FnName}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveFexitDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.FexitDetails) error {
	start := time.Now()
	_, err := s.stmtSaveFexitDetails.ExecContext(ctx, uint32(linkID), details.FnName)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveFexitDetails", "args", []any{linkID, details.FnName}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert fexit details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveFexitDetails", "args", []any{linkID, details.FnName}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveXDPDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.XDPDetails) error {
	proceedOnJSON, err := json.Marshal(details.ProceedOn)
	if err != nil {
		return fmt.Errorf("failed to marshal proceed_on: %w", err)
	}

	start := time.Now()
	_, err = s.stmtSaveXDPDetails.ExecContext(ctx,
		uint32(linkID), details.Interface, details.Ifindex, details.Priority, details.Position,
		string(proceedOnJSON), details.Netns, details.Nsid, details.DispatcherID, details.Revision)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveXDPDetails", "args", []any{linkID, details.Interface, details.Ifindex, details.Priority, details.Position, "(proceed_on)", details.Netns, details.Nsid, details.DispatcherID, details.Revision}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert xdp details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveXDPDetails", "args", []any{linkID, details.Interface, details.Ifindex, details.Priority, details.Position, "(proceed_on)", details.Netns, details.Nsid, details.DispatcherID, details.Revision}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveTCDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.TCDetails) error {
	proceedOnJSON, err := json.Marshal(details.ProceedOn)
	if err != nil {
		return fmt.Errorf("failed to marshal proceed_on: %w", err)
	}

	start := time.Now()
	_, err = s.stmtSaveTCDetails.ExecContext(ctx,
		uint32(linkID), details.Interface, details.Ifindex, details.Direction, details.Priority, details.Position,
		string(proceedOnJSON), details.Netns, details.Nsid, details.DispatcherID, details.Revision)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveTCDetails", "args", []any{linkID, details.Interface, details.Ifindex, details.Direction, details.Priority, details.Position, "(proceed_on)", details.Netns, details.Nsid, details.DispatcherID, details.Revision}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert tc details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveTCDetails", "args", []any{linkID, details.Interface, details.Ifindex, details.Direction, details.Priority, details.Position, "(proceed_on)", details.Netns, details.Nsid, details.DispatcherID, details.Revision}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

func (s *sqliteStore) saveTCXDetails(ctx context.Context, linkID bpfman.LinkID, details bpfman.TCXDetails) error {
	start := time.Now()
	_, err := s.stmtSaveTCXDetails.ExecContext(ctx,
		uint32(linkID), details.Interface, details.Ifindex, details.Direction, details.Priority, details.Netns, details.Nsid)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveTCXDetails", "args", []any{linkID, details.Interface, details.Ifindex, details.Direction, details.Priority, details.Netns, details.Nsid}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert tcx details: %w", err)
	}
	s.logger.Debug("sql", "stmt", "SaveTCXDetails", "args", []any{linkID, details.Interface, details.Ifindex, details.Direction, details.Priority, details.Netns, details.Nsid}, "duration_ms", msec(time.Since(start)), "rows_affected", 1)
	return nil
}

// ----------------------------------------------------------------------------
// Helper Functions
// ----------------------------------------------------------------------------

// insertLinkRegistry inserts a spec into the links table.
func (s *sqliteStore) insertLinkRegistry(ctx context.Context, spec bpfman.LinkSpec) error {
	start := time.Now()

	// Derive is_synthetic from the link ID
	isSynthetic := 0
	if spec.ID.IsSynthetic() {
		isSynthetic = 1
	}

	// Convert pin to nullable string for DB storage
	var pinPath sql.NullString
	if spec.PinPath != nil {
		pinPath = sql.NullString{String: spec.PinPath.String(), Valid: true}
	}

	_, err := s.stmtInsertLinkRegistry.ExecContext(ctx,
		uint32(spec.ID), string(spec.Kind), spec.ProgramID,
		pinPath, isSynthetic, spec.CreatedAt.Format(time.RFC3339))
	if err != nil {
		s.logger.Debug("sql", "stmt", "InsertLinkRegistry", "args", []any{spec.ID, spec.Kind, spec.ProgramID, pinPath, isSynthetic, "(timestamp)"}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert link: %w", err)
	}

	s.logger.Debug("sql", "stmt", "InsertLinkRegistry", "args", []any{spec.ID, spec.Kind, spec.ProgramID, pinPath, isSynthetic, "(timestamp)"}, "duration_ms", msec(time.Since(start)))
	return nil
}

// scanLinkRecord scans a single row into a LinkSpec (without details).
// Row format: link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
func (s *sqliteStore) scanLinkRecord(row *sql.Row) (bpfman.LinkSpec, error) {
	var record bpfman.LinkSpec
	var linkID int64
	var kindStr string
	var programID uint32
	var pinPath sql.NullString
	var isSynthetic int
	var createdAtStr string

	err := row.Scan(&linkID, &kindStr, &programID, &pinPath, &isSynthetic, &createdAtStr)
	if err == sql.ErrNoRows {
		return bpfman.LinkSpec{}, store.ErrNotFound
	}
	if err != nil {
		return bpfman.LinkSpec{}, err
	}

	record.ID = bpfman.LinkID(linkID)
	record.Kind = bpfman.LinkKind(kindStr)
	record.ProgramID = programID
	if pinPath.Valid {
		pin := bpffs.LinkPath(pinPath.String)
		record.PinPath = &pin
	}
	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return bpfman.LinkSpec{}, fmt.Errorf("invalid created_at timestamp for link %d: %q: %w", linkID, createdAtStr, err)
	}
	record.CreatedAt = createdAt

	return record, nil
}

// scanLinkRecords scans multiple rows into a slice of LinkSpec (without details).
// Row format: link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
func (s *sqliteStore) scanLinkRecords(rows *sql.Rows) ([]bpfman.LinkSpec, error) {
	var result []bpfman.LinkSpec

	for rows.Next() {
		var linkID int64
		var kindStr string
		var programID uint32
		var pinPath sql.NullString
		var isSynthetic int
		var createdAtStr string

		err := rows.Scan(&linkID, &kindStr, &programID, &pinPath, &isSynthetic, &createdAtStr)
		if err != nil {
			return nil, err
		}

		record := bpfman.LinkSpec{
			ID:        bpfman.LinkID(linkID),
			Kind:      bpfman.LinkKind(kindStr),
			ProgramID: programID,
		}
		if pinPath.Valid {
			pin := bpffs.LinkPath(pinPath.String)
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

// getLinkDetails retrieves the type-specific details for a link.
func (s *sqliteStore) getLinkDetails(ctx context.Context, kind bpfman.LinkKind, linkID bpfman.LinkID) (bpfman.LinkDetails, error) {
	switch kind {
	case bpfman.LinkKindTracepoint:
		return s.getTracepointDetails(ctx, linkID)
	case bpfman.LinkKindKprobe, bpfman.LinkKindKretprobe:
		return s.getKprobeDetails(ctx, linkID)
	case bpfman.LinkKindUprobe, bpfman.LinkKindUretprobe:
		return s.getUprobeDetails(ctx, linkID)
	case bpfman.LinkKindFentry:
		return s.getFentryDetails(ctx, linkID)
	case bpfman.LinkKindFexit:
		return s.getFexitDetails(ctx, linkID)
	case bpfman.LinkKindXDP:
		return s.getXDPDetails(ctx, linkID)
	case bpfman.LinkKindTC:
		return s.getTCDetails(ctx, linkID)
	case bpfman.LinkKindTCX:
		return s.getTCXDetails(ctx, linkID)
	default:
		return nil, fmt.Errorf("unknown link kind: %s", kind)
	}
}

func (s *sqliteStore) getTracepointDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.TracepointDetails, error) {
	start := time.Now()
	row := s.stmtGetTracepointDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.TracepointDetails
	err := row.Scan(&details.Group, &details.Name)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetTracepointDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.TracepointDetails{}, fmt.Errorf("tracepoint details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetTracepointDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.TracepointDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetTracepointDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)
	return details, nil
}

func (s *sqliteStore) getKprobeDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.KprobeDetails, error) {
	start := time.Now()
	row := s.stmtGetKprobeDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.KprobeDetails
	var retprobe int
	err := row.Scan(&details.FnName, &details.Offset, &retprobe)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetKprobeDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.KprobeDetails{}, fmt.Errorf("kprobe details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetKprobeDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.KprobeDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetKprobeDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)
	details.Retprobe = retprobe == 1
	return details, nil
}

func (s *sqliteStore) getUprobeDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.UprobeDetails, error) {
	start := time.Now()
	row := s.stmtGetUprobeDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.UprobeDetails
	var fnName sql.NullString
	var pid sql.NullInt64
	var retprobe int
	err := row.Scan(&details.Target, &fnName, &details.Offset, &pid, &retprobe)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetUprobeDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.UprobeDetails{}, fmt.Errorf("uprobe details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetUprobeDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.UprobeDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetUprobeDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)
	if fnName.Valid {
		details.FnName = fnName.String
	}
	if pid.Valid {
		details.PID = int32(pid.Int64)
	}
	details.Retprobe = retprobe == 1
	return details, nil
}

func (s *sqliteStore) getFentryDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.FentryDetails, error) {
	start := time.Now()
	row := s.stmtGetFentryDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.FentryDetails
	err := row.Scan(&details.FnName)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetFentryDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.FentryDetails{}, fmt.Errorf("fentry details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetFentryDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.FentryDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetFentryDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)
	return details, nil
}

func (s *sqliteStore) getFexitDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.FexitDetails, error) {
	start := time.Now()
	row := s.stmtGetFexitDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.FexitDetails
	err := row.Scan(&details.FnName)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetFexitDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.FexitDetails{}, fmt.Errorf("fexit details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetFexitDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.FexitDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetFexitDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)
	return details, nil
}

func (s *sqliteStore) getXDPDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.XDPDetails, error) {
	start := time.Now()
	row := s.stmtGetXDPDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.XDPDetails
	var proceedOnJSON string
	var netns sql.NullString
	err := row.Scan(&details.Interface, &details.Ifindex, &details.Priority, &details.Position,
		&proceedOnJSON, &netns, &details.Nsid, &details.DispatcherID, &details.Revision)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetXDPDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.XDPDetails{}, fmt.Errorf("xdp details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetXDPDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.XDPDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetXDPDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	if err := json.Unmarshal([]byte(proceedOnJSON), &details.ProceedOn); err != nil {
		return bpfman.XDPDetails{}, fmt.Errorf("failed to unmarshal proceed_on: %w", err)
	}
	if netns.Valid {
		details.Netns = netns.String
	}
	return details, nil
}

func (s *sqliteStore) getTCDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.TCDetails, error) {
	start := time.Now()
	row := s.stmtGetTCDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.TCDetails
	var proceedOnJSON string
	var netns sql.NullString
	err := row.Scan(&details.Interface, &details.Ifindex, &details.Direction, &details.Priority, &details.Position,
		&proceedOnJSON, &netns, &details.Nsid, &details.DispatcherID, &details.Revision)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetTCDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.TCDetails{}, fmt.Errorf("tc details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetTCDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.TCDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetTCDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	if err := json.Unmarshal([]byte(proceedOnJSON), &details.ProceedOn); err != nil {
		return bpfman.TCDetails{}, fmt.Errorf("failed to unmarshal proceed_on: %w", err)
	}
	if netns.Valid {
		details.Netns = netns.String
	}
	return details, nil
}

func (s *sqliteStore) getTCXDetails(ctx context.Context, linkID bpfman.LinkID) (bpfman.TCXDetails, error) {
	start := time.Now()
	row := s.stmtGetTCXDetails.QueryRowContext(ctx, int64(linkID))

	var details bpfman.TCXDetails
	var netns sql.NullString
	var nsid sql.NullInt64
	err := row.Scan(&details.Interface, &details.Ifindex, &details.Direction, &details.Priority, &netns, &nsid)
	if err == sql.ErrNoRows {
		s.logger.Debug("sql", "stmt", "GetTCXDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.TCXDetails{}, fmt.Errorf("tcx details for %d: %w", linkID, store.ErrNotFound)
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetTCXDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.TCXDetails{}, err
	}
	s.logger.Debug("sql", "stmt", "GetTCXDetails", "args", []any{linkID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	if netns.Valid {
		details.Netns = netns.String
	}
	if nsid.Valid {
		details.Nsid = uint64(nsid.Int64)
	}
	return details, nil
}
