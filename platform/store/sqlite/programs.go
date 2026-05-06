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

// Get retrieves program metadata by program ID.
// Returns platform.ErrRecordNotFound if the program does not exist.
func (s *sqliteStore) Get(ctx context.Context, programID kernel.ProgramID) (bpfman.ProgramRecord, error) {
	start := time.Now()
	row := s.stmtGetProgram.QueryRowContext(ctx, programID)

	prog, err := s.scanProgram(row)
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Debug("sql", "stmt", "GetProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "rows", 0)
		return bpfman.ProgramRecord{}, platform.ErrRecordNotFound
	}
	if err != nil {
		s.logger.Debug("sql", "stmt", "GetProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "error", err)
		return bpfman.ProgramRecord{}, err
	}
	s.logger.Debug("sql", "stmt", "GetProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "rows", 1)

	prog.ProgramID = programID
	return prog, nil
}

// scannedProgram holds the raw column values from a program row scan.
// Both scanProgram and scanProgramFromRows populate this struct, then
// delegate to buildProgramRecord for the shared parsing logic.
type scannedProgram struct {
	programName, programTypeStr, objectPath, pinPath string
	attachFunc, globalDataJSON, mapPinPath           sql.NullString
	imageSourceJSON, owner, description              sql.NullString
	license, metadataJSON                            sql.NullString
	mapOwnerID                                       sql.NullInt64
	gplCompatible                                    int
	createdAtStr, updatedAtStr                       string
}

// buildProgramRecord converts scanned column values into a
// ProgramRecord. It handles program type parsing, nullable scalar
// field extraction, JSON unmarshalling, and timestamp parsing.
func buildProgramRecord(sp *scannedProgram) (bpfman.ProgramRecord, error) {
	programType, err := bpfman.ParseProgramType(sp.programTypeStr)
	if err != nil {
		return bpfman.ProgramRecord{}, fmt.Errorf("invalid program type: %q: %w", sp.programTypeStr, err)
	}

	var attachFuncVal string
	var mapOwnerIDPtr *kernel.ProgramID
	var mapPinPathVal string
	if sp.attachFunc.Valid {
		attachFuncVal = sp.attachFunc.String
	}
	if sp.mapOwnerID.Valid {
		v := kernel.ProgramID(sp.mapOwnerID.Int64)
		mapOwnerIDPtr = &v
	}
	if sp.mapPinPath.Valid {
		mapPinPathVal = sp.mapPinPath.String
	}

	var globalData map[string][]byte
	var imageURL, imageDigest string
	var imagePullPolicy bpfman.ImagePullPolicy
	var metadata map[string]string
	if sp.globalDataJSON.Valid {
		if err := json.Unmarshal([]byte(sp.globalDataJSON.String), &globalData); err != nil {
			return bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal global_data: %w", err)
		}
	}
	if sp.imageSourceJSON.Valid {
		var imgSrc struct {
			URL        string                 `json:"url"`
			Digest     string                 `json:"digest,omitempty"`
			PullPolicy bpfman.ImagePullPolicy `json:"pull_policy"`
		}
		if err := json.Unmarshal([]byte(sp.imageSourceJSON.String), &imgSrc); err != nil {
			return bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal image_source: %w", err)
		}
		imageURL = imgSrc.URL
		imageDigest = imgSrc.Digest
		imagePullPolicy = imgSrc.PullPolicy
		if !imagePullPolicy.Valid() {
			return bpfman.ProgramRecord{}, fmt.Errorf("invalid image pull policy in image_source for program %q", sp.programName)
		}
	}
	if sp.metadataJSON.Valid && sp.metadataJSON.String != "" {
		if err := json.Unmarshal([]byte(sp.metadataJSON.String), &metadata); err != nil {
			return bpfman.ProgramRecord{}, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}

	createdAt, err := time.Parse(time.RFC3339, sp.createdAtStr)
	if err != nil {
		return bpfman.ProgramRecord{}, fmt.Errorf("invalid created_at timestamp %q: %w", sp.createdAtStr, err)
	}
	updatedAt, err := time.Parse(time.RFC3339, sp.updatedAtStr)
	if err != nil {
		return bpfman.ProgramRecord{}, fmt.Errorf("invalid updated_at timestamp %q: %w", sp.updatedAtStr, err)
	}

	var licenseVal string
	if sp.license.Valid {
		licenseVal = sp.license.String
	}

	prog := bpfman.ProgramRecord{
		Load: bpfman.LoadSpec{}.
			WithObjectPath(sp.objectPath).
			WithProgramName(sp.programName).
			WithProgramType(programType).
			WithGlobalData(globalData).
			WithImageProvenance(imageURL, imageDigest, imagePullPolicy).
			WithAttachFunc(attachFuncVal),
		License:       licenseVal,
		GPLCompatible: sp.gplCompatible != 0,
		Handles: bpfman.ProgramHandles{
			PinPath:    bpfman.ProgPinPath(sp.pinPath),
			MapsDir:    mapPinPathVal,
			MapOwnerID: mapOwnerIDPtr,
		},
		Meta: bpfman.ProgramMeta{
			Name:     sp.programName,
			Metadata: metadata,
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if sp.owner.Valid {
		prog.Meta.Owner = sp.owner.String
	}
	if sp.description.Valid {
		prog.Meta.Description = sp.description.String
	}

	return prog, nil
}

// scanProgram scans a single row into a ProgramRecord struct.
func (s *sqliteStore) scanProgram(row *sql.Row) (bpfman.ProgramRecord, error) {
	var sp scannedProgram
	err := row.Scan(
		&sp.programName,
		&sp.programTypeStr,
		&sp.objectPath,
		&sp.pinPath,
		&sp.attachFunc,
		&sp.globalDataJSON,
		&sp.mapOwnerID,
		&sp.mapPinPath,
		&sp.imageSourceJSON,
		&sp.owner,
		&sp.description,
		&sp.license,
		&sp.gplCompatible,
		&sp.createdAtStr,
		&sp.updatedAtStr,
		&sp.metadataJSON,
	)
	if err != nil {
		return bpfman.ProgramRecord{}, err
	}
	return buildProgramRecord(&sp)
}

// Save stores program metadata using last-write-wins upsert semantics.
//
// If a row with the same program_id already exists it is overwritten
// rather than rejected. This is necessary because the kernel reuses
// program IDs aggressively after unload, so a collision may simply
// mean the ID was recycled rather than indicating a bug.
//
// On overwrite the original created_at is preserved and updated_at
// is set to the current time so that created_at != updated_at serves
// as a clear signal that the program_id was reused.
//
// For atomicity with other operations, wrap in RunInTransaction.
func (s *sqliteStore) Save(ctx context.Context, programID kernel.ProgramID, metadata bpfman.ProgramRecord) error {
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
			PullPolicy bpfman.ImagePullPolicy `json:"pull_policy"`
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
	if metadata.Handles.MapsDir != "" {
		mapPinPath = sql.NullString{String: metadata.Handles.MapsDir, Valid: true}
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
		programID,
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
		s.logger.Debug("sql", "stmt", "SaveProgram", "args", []any{programID, metadata.Meta.Name, "(columns)"}, "duration_ms", msec(time.Since(start)), "error", err)
		return fmt.Errorf("failed to insert program: %w", err)
	}
	rows, _ := result.RowsAffected()
	s.logger.Debug("sql", "stmt", "SaveProgram", "args", []any{programID, metadata.Meta.Name, "(columns)"}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)

	return nil
}

// Delete removes program metadata. Returns ErrRecordNotFound if the
// program does not exist.
func (s *sqliteStore) Delete(ctx context.Context, programID kernel.ProgramID) error {
	start := time.Now()
	result, err := s.stmtDeleteProgram.ExecContext(ctx, programID)
	if err != nil {
		s.logger.Debug("sql", "stmt", "DeleteProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "error", err)
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	s.logger.Debug("sql", "stmt", "DeleteProgram", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "rows_affected", rows)
	if rows == 0 {
		return fmt.Errorf("program %d: %w", programID, platform.ErrRecordNotFound)
	}
	return nil
}

// CountDependentPrograms returns the number of programs that share maps with
// the given program (i.e., programs where map_owner_id = programID).
func (s *sqliteStore) CountDependentPrograms(ctx context.Context, programID kernel.ProgramID) (int, error) {
	start := time.Now()
	var count int
	err := s.stmtCountDependentPrograms.QueryRowContext(ctx, programID).Scan(&count)
	if err != nil {
		s.logger.Debug("sql", "stmt", "CountDependentPrograms", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "error", err)
		return 0, err
	}
	s.logger.Debug("sql", "stmt", "CountDependentPrograms", "args", []any{programID}, "duration_ms", msec(time.Since(start)), "count", count)
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
		programID, prog, err := s.scanProgramFromRows(rows)
		if err != nil {
			return nil, err
		}
		prog.ProgramID = programID
		result[programID] = prog
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logger.Debug("sql", "stmt", "ListPrograms", "duration_ms", msec(time.Since(start)), "rows", len(result))
	return result, nil
}

// scanProgramFromRows scans a single row from *sql.Rows into a ProgramRecord struct.
// The row must include the program_id column followed by the standard columns.
func (s *sqliteStore) scanProgramFromRows(rows *sql.Rows) (kernel.ProgramID, bpfman.ProgramRecord, error) {
	var programID kernel.ProgramID
	var sp scannedProgram
	err := rows.Scan(
		&programID,
		&sp.programName,
		&sp.programTypeStr,
		&sp.objectPath,
		&sp.pinPath,
		&sp.attachFunc,
		&sp.globalDataJSON,
		&sp.mapOwnerID,
		&sp.mapPinPath,
		&sp.imageSourceJSON,
		&sp.owner,
		&sp.description,
		&sp.license,
		&sp.gplCompatible,
		&sp.createdAtStr,
		&sp.updatedAtStr,
		&sp.metadataJSON,
	)
	if err != nil {
		return 0, bpfman.ProgramRecord{}, err
	}
	prog, err := buildProgramRecord(&sp)
	if err != nil {
		return 0, bpfman.ProgramRecord{}, err
	}
	return programID, prog, nil
}
