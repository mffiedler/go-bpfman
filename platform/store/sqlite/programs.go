package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// Get retrieves program metadata by kernel ID.
// Returns platform.ErrRecordNotFound if the program does not exist.
func (s *sqliteStore) Get(ctx context.Context, kernelID kernel.ProgramID) (bpfman.ProgramRecord, error) {
	start := time.Now()
	row := s.stmtGetProgram.QueryRowContext(ctx, kernelID)

	prog, err := s.scanProgram(row)
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Debug("sql", "stmt", "GetProgram", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.ProgramRecord{}, platform.ErrRecordNotFound
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetProgram", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.ProgramRecord{}, err
	}
	s.logger.Debug("sql", "stmt", "GetProgram", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	prog.KernelID = kernelID
	return prog, nil
}

// scanProgram scans a single row into a ProgramRecord struct.
func (s *sqliteStore) scanProgram(row *sql.Row) (bpfman.ProgramRecord, error) {
	var programName, programTypeStr, objectPath, pinPath string
	var attachFunc, globalDataJSON, mapPinPath, imageSourceJSON, owner, description, license, metadataJSON sql.NullString
	var mapOwnerID sql.NullInt64
	var gplCompatible int
	var createdAtStr, updatedAtStr string

	err := row.Scan(
		&programName,
		&programTypeStr,
		&objectPath,
		&pinPath,
		&attachFunc,
		&globalDataJSON,
		&mapOwnerID,
		&mapPinPath,
		&imageSourceJSON,
		&owner,
		&description,
		&license,
		&gplCompatible,
		&createdAtStr,
		&updatedAtStr,
		&metadataJSON,
	)
	if err != nil {
		return bpfman.ProgramRecord{}, err
	}

	// Parse program type
	programType, ok := bpfman.ParseProgramType(programTypeStr)
	if !ok {
		return bpfman.ProgramRecord{}, fmt.Errorf("invalid program type: %q", programTypeStr)
	}

	// Parse nullable scalar fields
	var attachFuncVal string
	var mapOwnerIDPtr *kernel.ProgramID
	var mapPinPathVal string
	if attachFunc.Valid {
		attachFuncVal = attachFunc.String
	}
	if mapOwnerID.Valid {
		v := kernel.ProgramID(mapOwnerID.Int64)
		mapOwnerIDPtr = &v
	}
	if mapPinPath.Valid {
		mapPinPathVal = mapPinPath.String
	}

	// Parse JSON fields
	var globalData map[string][]byte
	var imageURL, imageDigest string
	var imagePullPolicy bpfman.ImagePullPolicy
	var metadata map[string]string
	if globalDataJSON.Valid {
		if err := json.Unmarshal([]byte(globalDataJSON.String), &globalData); err != nil {
			return bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal global_data: %w", err)
		}
	}
	if imageSourceJSON.Valid {
		var imgSrc struct {
			URL        string                 `json:"url"`
			Digest     string                 `json:"digest,omitempty"`
			PullPolicy bpfman.ImagePullPolicy `json:"pull_policy,omitempty"`
		}
		if err := json.Unmarshal([]byte(imageSourceJSON.String), &imgSrc); err != nil {
			return bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal image_source: %w", err)
		}
		imageURL = imgSrc.URL
		imageDigest = imgSrc.Digest
		imagePullPolicy = imgSrc.PullPolicy
	}
	if metadataJSON.Valid && metadataJSON.String != "" {
		if err := json.Unmarshal([]byte(metadataJSON.String), &metadata); err != nil {
			return bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}

	// Parse timestamps
	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return bpfman.ProgramRecord{}, fmt.Errorf("invalid created_at timestamp %q: %w", createdAtStr, err)
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedAtStr)
	if err != nil {
		return bpfman.ProgramRecord{}, fmt.Errorf("invalid updated_at timestamp %q: %w", updatedAtStr, err)
	}

	// Parse license field
	var licenseVal string
	if license.Valid {
		licenseVal = license.String
	}

	// Build the ProgramRecord from the stored fields using nested structs
	prog := bpfman.ProgramRecord{
		Load: bpfman.LoadSpec{}.
			WithObjectPath(objectPath).
			WithProgramName(programName).
			WithProgramType(programType).
			WithGlobalData(globalData).
			WithImageProvenance(imageURL, imageDigest, imagePullPolicy).
			WithAttachFunc(attachFuncVal),
		License:       licenseVal,
		GPLCompatible: gplCompatible != 0,
		Handles: bpfman.ProgramHandles{
			PinPath:    pinPath,
			MapPinPath: mapPinPathVal,
			MapOwnerID: mapOwnerIDPtr,
		},
		Meta: bpfman.ProgramMeta{
			Name:     programName,
			Metadata: metadata,
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if owner.Valid {
		prog.Meta.Owner = owner.String
	}
	if description.Valid {
		prog.Meta.Description = description.String
	}

	return prog, nil
}

// Save stores program metadata using last-write-wins upsert semantics.
//
// If a row with the same kernel_id already exists it is overwritten
// rather than rejected. This is necessary because the kernel reuses
// program IDs aggressively after unload, so a collision may simply
// mean the ID was recycled rather than indicating a bug.
//
// On overwrite the original created_at is preserved and updated_at
// is set to the current time so that created_at != updated_at serves
// as a clear signal that the kernel_id was reused.
//
// For atomicity with other operations, wrap in RunInTransaction.
func (s *sqliteStore) Save(ctx context.Context, kernelID kernel.ProgramID, metadata bpfman.ProgramRecord) error {
	// Marshal JSON fields
	var globalDataJSON, imageSourceJSON sql.NullString
	if metadata.Load.GlobalData() != nil {
		data, err := json.Marshal(metadata.Load.GlobalData())
		if err != nil {
			return fmt.Errorf("failed to marshal global_data: %w", err)
		}
		globalDataJSON = sql.NullString{String: string(data), Valid: true}
	}
	if metadata.Load.HasImageSource() {
		imgSrc := struct {
			URL        string                 `json:"url"`
			Digest     string                 `json:"digest,omitempty"`
			PullPolicy bpfman.ImagePullPolicy `json:"pull_policy,omitempty"`
		}{
			URL:        metadata.Load.ImageURL(),
			Digest:     metadata.Load.ImageDigest(),
			PullPolicy: metadata.Load.ImagePullPolicy(),
		}
		data, err := json.Marshal(imgSrc)
		if err != nil {
			return fmt.Errorf("failed to marshal image_source: %w", err)
		}
		imageSourceJSON = sql.NullString{String: string(data), Valid: true}
	}

	// Marshal metadata as JSON
	metadataJSON := "{}"
	if metadata.Meta.Metadata != nil {
		data, err := json.Marshal(metadata.Meta.Metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
		metadataJSON = string(data)
	}

	// Handle nullable fields
	var mapOwnerID sql.NullInt64
	if metadata.Handles.MapOwnerID != nil {
		mapOwnerID = sql.NullInt64{Int64: int64(*metadata.Handles.MapOwnerID), Valid: true}
	}
	var mapPinPath sql.NullString
	if metadata.Handles.MapPinPath != "" {
		mapPinPath = sql.NullString{String: metadata.Handles.MapPinPath, Valid: true}
	}
	var attachFunc, owner, description, license sql.NullString
	if metadata.Load.AttachFunc() != "" {
		attachFunc = sql.NullString{String: metadata.Load.AttachFunc(), Valid: true}
	}
	if metadata.Meta.Owner != "" {
		owner = sql.NullString{String: metadata.Meta.Owner, Valid: true}
	}
	if metadata.Meta.Description != "" {
		description = sql.NullString{String: metadata.Meta.Description, Valid: true}
	}
	if metadata.License != "" {
		license = sql.NullString{String: metadata.License, Valid: true}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Convert bool to int for SQLite
	var gplCompatibleInt int
	if metadata.GPLCompatible {
		gplCompatibleInt = 1
	}

	start := time.Now()
	result, err := s.stmtSaveProgram.ExecContext(ctx,
		kernelID,
		metadata.Meta.Name,
		metadata.Load.ProgramType().String(),
		metadata.Load.ObjectPath(),
		metadata.Handles.PinPath,
		attachFunc,
		globalDataJSON,
		mapOwnerID,
		mapPinPath,
		imageSourceJSON,
		owner,
		description,
		license,
		gplCompatibleInt,
		metadataJSON,
		metadata.CreatedAt.Format(time.RFC3339),
		now,
	)
	if err != nil {
		s.logger.Debug("sql", "stmt", "SaveProgram", "args", []any{kernelID, metadata.Meta.Name, "(columns)"}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert program: %w", err)
	}
	rows, _ := result.RowsAffected()
	s.logger.Debug("sql", "stmt", "SaveProgram", "args", []any{kernelID, metadata.Meta.Name, "(columns)"}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)

	return nil
}

// Delete removes program metadata.
func (s *sqliteStore) Delete(ctx context.Context, kernelID kernel.ProgramID) error {
	start := time.Now()
	result, err := s.stmtDeleteProgram.ExecContext(ctx, kernelID)
	if err != nil {
		s.logger.Debug("sql", "stmt", "DeleteProgram", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "error", err)
		return err
	}
	rows, _ := result.RowsAffected()
	s.logger.Debug("sql", "stmt", "DeleteProgram", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)
	return nil
}

// GC removes all stale entries (programs, dispatchers, links) that don't
// exist in the provided kernel state. Handles internal ordering constraints
// (e.g., dependent programs before map owners for FK constraints).
//
// All deletions run within a single transaction so the store is never
// left in a partially collected state.
func (s *sqliteStore) GC(ctx context.Context, kernelProgramIDs map[kernel.ProgramID]bool, kernelLinkIDs map[kernel.LinkID]bool) (platform.GCResult, error) {
	start := time.Now()
	var result platform.GCResult

	err := s.RunInTransaction(ctx, func(txStore platform.Store) error {
		ts := txStore.(*sqliteStore)

		// 1. GC programs (order: dependents before owners)
		stored, err := ts.List(ctx)
		if err != nil {
			return fmt.Errorf("list programs: %w", err)
		}

		var dependents, owners []kernel.ProgramID
		for id, prog := range stored {
			if !kernelProgramIDs[id] {
				if prog.Handles.MapOwnerID != nil {
					dependents = append(dependents, id)
				} else {
					owners = append(owners, id)
				}
			}
		}

		for _, id := range dependents {
			if err := ts.Delete(ctx, id); err != nil {
				s.logger.Warn("failed to delete dependent program", "kernel_id", id, "error", err)
				continue
			}
			result.ProgramsRemoved++
		}
		for _, id := range owners {
			if err := ts.Delete(ctx, id); err != nil {
				s.logger.Warn("failed to delete owner program", "kernel_id", id, "error", err)
				continue
			}
			result.ProgramsRemoved++
		}

		// 2. Reconcile dispatchers (delete those referencing gone programs)
		dispatchers, err := ts.ListDispatchers(ctx)
		if err != nil {
			return fmt.Errorf("list dispatchers: %w", err)
		}

		for _, disp := range dispatchers {
			if !kernelProgramIDs[disp.KernelID] {
				if err := ts.DeleteDispatcher(ctx, string(disp.Type), disp.Nsid, disp.Ifindex); err != nil {
					s.logger.Warn("failed to delete dispatcher", "type", disp.Type, "nsid", disp.Nsid, "ifindex", disp.Ifindex, "error", err)
					continue
				}
				result.DispatchersRemoved++
			}
		}

		// 3. Reconcile links (delete those not in kernel)
		// Skip synthetic link IDs (>= 0x80000000) since they're not real kernel links
		// and cannot be enumerated via the kernel's link iterator. These are used for
		// perf_event-based attachments (e.g., container uprobes) that lack kernel link IDs.
		links, err := ts.ListLinks(ctx)
		if err != nil {
			return fmt.Errorf("list links: %w", err)
		}

		for _, link := range links {
			// Skip synthetic links (perf_event-based) - they don't have kernel link IDs
			// and are not subject to kernel-based GC
			if link.IsSynthetic() {
				continue
			}
			// For non-synthetic links, ID is the kernel link ID
			if !kernelLinkIDs[link.ID] {
				if err := ts.DeleteLink(ctx, link.ID); err != nil {
					s.logger.Warn("failed to delete link", "link_id", link.ID, "error", err)
					continue
				}
				result.LinksRemoved++
			}
		}

		// 4. Reconcile dispatchers after link GC: delete any dispatcher
		// that has no remaining extension links so the next attach
		// recreates a fresh dispatcher.
		if result.LinksRemoved > 0 {
			surviving, err := ts.ListDispatchers(ctx)
			if err != nil {
				return fmt.Errorf("list dispatchers after link GC: %w", err)
			}
			for _, disp := range surviving {
				liveLinks, err := ts.CountDispatcherLinks(ctx, disp.KernelID)
				if err != nil {
					s.logger.Warn("failed to count dispatcher links", "kernel_id", disp.KernelID, "error", err)
					continue
				}
				if liveLinks == 0 {
					s.logger.Info("deleting dispatcher with no live extensions",
						"type", disp.Type, "nsid", disp.Nsid, "ifindex", disp.Ifindex,
						"kernel_id", disp.KernelID)
					if err := ts.DeleteDispatcher(ctx, string(disp.Type), disp.Nsid, disp.Ifindex); err != nil {
						s.logger.Warn("failed to delete stale dispatcher", "kernel_id", disp.KernelID, "error", err)
						continue
					}
					result.DispatchersRemoved++
				}
			}
		}

		return nil
	})

	s.logger.Debug("reconcile", "duration_ms", msec(time.Since(start)),
		"programs_removed", result.ProgramsRemoved,
		"dispatchers_removed", result.DispatchersRemoved,
		"links_removed", result.LinksRemoved)

	return result, err
}

// CountDependentPrograms returns the number of programs that share maps with
// the given program (i.e., programs where map_owner_id = kernelID).
func (s *sqliteStore) CountDependentPrograms(ctx context.Context, kernelID kernel.ProgramID) (int, error) {
	start := time.Now()
	var count int
	err := s.stmtCountDependentPrograms.QueryRowContext(ctx, kernelID).Scan(&count)
	if err != nil {
		s.logger.Debug("sql", "stmt", "CountDependentPrograms", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, err
	}
	s.logger.Debug("sql", "stmt", "CountDependentPrograms", "args", []any{kernelID}, "duration_ms", msec(time.Since(start)), "count", count)
	return count, nil
}

// List returns all program metadata. The returned map has no guaranteed
// iteration order; sorting for deterministic output is done in inspect.Snapshot.
func (s *sqliteStore) List(ctx context.Context) (map[kernel.ProgramID]bpfman.ProgramRecord, error) {
	start := time.Now()
	rows, err := s.stmtListPrograms.QueryContext(ctx)
	if err != nil {
		s.logger.Debug("sql", "stmt", "ListPrograms", "duration_ms", msec(time.Since(start)), "error", err)
		return nil, err
	}
	defer rows.Close()

	result := make(map[kernel.ProgramID]bpfman.ProgramRecord)
	for rows.Next() {
		kernelID, prog, err := s.scanProgramFromRows(rows)
		if err != nil {
			return nil, err
		}
		prog.KernelID = kernelID
		result[kernelID] = prog
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logger.Debug("sql", "stmt", "ListPrograms", "duration_ms", msec(time.Since(start)), "rows", len(result))
	return result, nil
}

// scanProgramFromRows scans a single row from *sql.Rows into a ProgramRecord struct.
// The row must include the tags and metadata columns.
func (s *sqliteStore) scanProgramFromRows(rows *sql.Rows) (kernel.ProgramID, bpfman.ProgramRecord, error) {
	var kernelID kernel.ProgramID
	var programName, programTypeStr, objectPath, pinPath string
	var attachFunc, globalDataJSON, mapPinPath, imageSourceJSON, owner, description, license, metadataJSON sql.NullString
	var mapOwnerID sql.NullInt64
	var gplCompatible int
	var createdAtStr, updatedAtStr string

	err := rows.Scan(
		&kernelID,
		&programName,
		&programTypeStr,
		&objectPath,
		&pinPath,
		&attachFunc,
		&globalDataJSON,
		&mapOwnerID,
		&mapPinPath,
		&imageSourceJSON,
		&owner,
		&description,
		&license,
		&gplCompatible,
		&createdAtStr,
		&updatedAtStr,
		&metadataJSON,
	)
	if err != nil {
		return 0, bpfman.ProgramRecord{}, err
	}

	// Parse program type
	programType, ok := bpfman.ParseProgramType(programTypeStr)
	if !ok {
		return 0, bpfman.ProgramRecord{}, fmt.Errorf("invalid program type for %d: %q", kernelID, programTypeStr)
	}

	// Parse nullable scalar fields
	var attachFuncVal string
	var mapOwnerIDPtr *kernel.ProgramID
	var mapPinPathVal string
	if attachFunc.Valid {
		attachFuncVal = attachFunc.String
	}
	if mapOwnerID.Valid {
		v := kernel.ProgramID(mapOwnerID.Int64)
		mapOwnerIDPtr = &v
	}
	if mapPinPath.Valid {
		mapPinPathVal = mapPinPath.String
	}

	// Parse JSON fields
	var globalData map[string][]byte
	var imageURL, imageDigest string
	var imagePullPolicy bpfman.ImagePullPolicy
	var metadata map[string]string
	if globalDataJSON.Valid {
		if err := json.Unmarshal([]byte(globalDataJSON.String), &globalData); err != nil {
			return 0, bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal global_data for %d: %w", kernelID, err)
		}
	}
	if imageSourceJSON.Valid {
		var imgSrc struct {
			URL        string                 `json:"url"`
			Digest     string                 `json:"digest,omitempty"`
			PullPolicy bpfman.ImagePullPolicy `json:"pull_policy,omitempty"`
		}
		if err := json.Unmarshal([]byte(imageSourceJSON.String), &imgSrc); err != nil {
			return 0, bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal image_source for %d: %w", kernelID, err)
		}
		imageURL = imgSrc.URL
		imageDigest = imgSrc.Digest
		imagePullPolicy = imgSrc.PullPolicy
	}
	if metadataJSON.Valid && metadataJSON.String != "" {
		if err := json.Unmarshal([]byte(metadataJSON.String), &metadata); err != nil {
			return 0, bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal metadata for %d: %w", kernelID, err)
		}
	}

	// Parse timestamps
	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return 0, bpfman.ProgramRecord{}, fmt.Errorf("invalid created_at timestamp for %d: %q: %w", kernelID, createdAtStr, err)
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedAtStr)
	if err != nil {
		return 0, bpfman.ProgramRecord{}, fmt.Errorf("invalid updated_at timestamp for %d: %q: %w", kernelID, updatedAtStr, err)
	}

	// Parse license field
	var licenseVal string
	if license.Valid {
		licenseVal = license.String
	}

	// Build the ProgramRecord from the stored fields using nested structs
	prog := bpfman.ProgramRecord{
		KernelID: kernelID,
		Load: bpfman.LoadSpec{}.
			WithObjectPath(objectPath).
			WithProgramName(programName).
			WithProgramType(programType).
			WithGlobalData(globalData).
			WithImageProvenance(imageURL, imageDigest, imagePullPolicy).
			WithAttachFunc(attachFuncVal),
		License:       licenseVal,
		GPLCompatible: gplCompatible != 0,
		Handles: bpfman.ProgramHandles{
			PinPath:    pinPath,
			MapPinPath: mapPinPathVal,
			MapOwnerID: mapOwnerIDPtr,
		},
		Meta: bpfman.ProgramMeta{
			Name:     programName,
			Metadata: metadata,
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if owner.Valid {
		prog.Meta.Owner = owner.String
	}
	if description.Valid {
		prog.Meta.Description = description.String
	}

	return kernelID, prog, nil
}
