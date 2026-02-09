package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/frobware/go-bpfman/platform"
)

// msec formats a duration as milliseconds with 3 decimal places.
func msec(d time.Duration) string {
	return fmt.Sprintf("%.3f", float64(d.Microseconds())/1000)
}

//go:embed schema.sql
var schemaSQL string

// dbConn abstracts *sql.DB and *sql.Tx for query execution.
type dbConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqliteStore implements platform.Store using SQLite.
type sqliteStore struct {
	db     *sql.DB // original connection, used for BeginTx
	conn   dbConn  // active connection (db or tx)
	logger *slog.Logger

	// Prepared statements for program operations
	stmtGetProgram             *sql.Stmt
	stmtSaveProgram            *sql.Stmt
	stmtDeleteProgram          *sql.Stmt
	stmtListPrograms           *sql.Stmt
	stmtCountDependentPrograms *sql.Stmt

	// Prepared statements for link registry operations
	stmtDeleteLink         *sql.Stmt
	stmtGetLinkRegistry    *sql.Stmt
	stmtListLinks          *sql.Stmt
	stmtListLinksByProgram *sql.Stmt
	stmtInsertLinkRegistry *sql.Stmt

	// Prepared statements for TCX link queries
	stmtListTCXLinksByInterface *sql.Stmt

	// Prepared statements for link detail queries
	stmtGetTracepointDetails *sql.Stmt
	stmtGetKprobeDetails     *sql.Stmt
	stmtGetUprobeDetails     *sql.Stmt
	stmtGetFentryDetails     *sql.Stmt
	stmtGetFexitDetails      *sql.Stmt
	stmtGetXDPDetails        *sql.Stmt
	stmtGetTCDetails         *sql.Stmt
	stmtGetTCXDetails        *sql.Stmt

	// Prepared statements for link detail inserts
	stmtSaveTracepointDetails *sql.Stmt
	stmtSaveKprobeDetails     *sql.Stmt
	stmtSaveUprobeDetails     *sql.Stmt
	stmtSaveFentryDetails     *sql.Stmt
	stmtSaveFexitDetails      *sql.Stmt
	stmtSaveXDPDetails        *sql.Stmt
	stmtSaveTCDetails         *sql.Stmt
	stmtSaveTCXDetails        *sql.Stmt

	// Prepared statements for batch link detail queries (used by ListLinks)
	stmtListAllTracepointDetails *sql.Stmt
	stmtListAllKprobeDetails     *sql.Stmt
	stmtListAllUprobeDetails     *sql.Stmt
	stmtListAllFentryDetails     *sql.Stmt
	stmtListAllFexitDetails      *sql.Stmt
	stmtListAllXDPDetails        *sql.Stmt
	stmtListAllTCDetails         *sql.Stmt
	stmtListAllTCXDetails        *sql.Stmt

	// Prepared statements for dispatcher operations
	stmtGetDispatcher        *sql.Stmt
	stmtListDispatchers      *sql.Stmt
	stmtSaveDispatcher       *sql.Stmt
	stmtDeleteDispatcher     *sql.Stmt
	stmtIncrementRevision    *sql.Stmt
	stmtGetDispatcherByType  *sql.Stmt
	stmtCountDispatcherLinks *sql.Stmt
}

// New creates a new SQLite store at the given path.
// If the schema version doesn't match, the database is deleted and recreated.
func New(ctx context.Context, dbPath string, logger *slog.Logger) (platform.Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "store", "db", dbPath)

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open(driverName, dsn(dbPath, [][2]string{{"journal_mode", "WAL"}, {"foreign_keys", "1"}}))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	s := &sqliteStore{db: db, conn: db, logger: logger}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		// Schema version mismatch - delete and recreate
		if strings.Contains(err.Error(), "schema version mismatch") {
			logger.Warn("schema version mismatch, recreating database", "error", err)
			if err := deleteDatabase(dbPath); err != nil {
				return nil, fmt.Errorf("failed to delete old database: %w", err)
			}
			return New(ctx, dbPath, logger) // Recursive call with fresh DB
		}
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}
	if err := s.prepareStatements(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to prepare statements for %s: %w", dbPath, err)
	}

	logger.Info("opened database", "path", dbPath)
	return s, nil
}

// deleteDatabase removes the SQLite database file and its WAL/SHM companions.
func deleteDatabase(dbPath string) error {
	// Remove main database file
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Remove WAL file if present
	if err := os.Remove(dbPath + "-wal"); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Remove SHM file if present
	if err := os.Remove(dbPath + "-shm"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewInMemory creates an in-memory SQLite store for testing.
func NewInMemory(ctx context.Context, logger *slog.Logger) (platform.Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "store", "db", ":memory:")

	db, err := sql.Open(driverName, dsn(":memory:", [][2]string{{"foreign_keys", "1"}}))
	if err != nil {
		return nil, fmt.Errorf("failed to open in-memory database: %w", err)
	}

	s := &sqliteStore{db: db, conn: db, logger: logger}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}
	if err := s.prepareStatements(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to prepare statements: %w", err)
	}

	logger.Info("opened in-memory database")
	return s, nil
}

// Close closes all prepared statements and the database connection.
func (s *sqliteStore) Close() error {
	s.closeStatements()
	return s.db.Close()
}

// closeStatements closes all prepared statements. Each close error
// is silently ignored because the database is about to be closed.
func (s *sqliteStore) closeStatements() {
	stmts := []*sql.Stmt{
		s.stmtGetProgram,
		s.stmtSaveProgram,
		s.stmtDeleteProgram,
		s.stmtListPrograms,
		s.stmtCountDependentPrograms,
		s.stmtDeleteLink,
		s.stmtGetLinkRegistry,
		s.stmtListLinks,
		s.stmtListLinksByProgram,
		s.stmtInsertLinkRegistry,
		s.stmtListTCXLinksByInterface,
		s.stmtGetTracepointDetails,
		s.stmtGetKprobeDetails,
		s.stmtGetUprobeDetails,
		s.stmtGetFentryDetails,
		s.stmtGetFexitDetails,
		s.stmtGetXDPDetails,
		s.stmtGetTCDetails,
		s.stmtGetTCXDetails,
		s.stmtSaveTracepointDetails,
		s.stmtSaveKprobeDetails,
		s.stmtSaveUprobeDetails,
		s.stmtSaveFentryDetails,
		s.stmtSaveFexitDetails,
		s.stmtSaveXDPDetails,
		s.stmtSaveTCDetails,
		s.stmtSaveTCXDetails,
		s.stmtListAllTracepointDetails,
		s.stmtListAllKprobeDetails,
		s.stmtListAllUprobeDetails,
		s.stmtListAllFentryDetails,
		s.stmtListAllFexitDetails,
		s.stmtListAllXDPDetails,
		s.stmtListAllTCDetails,
		s.stmtListAllTCXDetails,
		s.stmtGetDispatcher,
		s.stmtListDispatchers,
		s.stmtSaveDispatcher,
		s.stmtDeleteDispatcher,
		s.stmtIncrementRevision,
		s.stmtGetDispatcherByType,
		s.stmtCountDispatcherLinks,
	}
	for _, stmt := range stmts {
		if stmt != nil {
			stmt.Close()
		}
	}
}

// schemaVersion is the current schema version. Increment this when the schema changes.
// Migrations are supported from version 2 onwards.
const schemaVersion = 4

func (s *sqliteStore) migrate(ctx context.Context) error {
	// Check current schema version
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("failed to read schema version: %w", err)
	}

	// Handle version 2 -> 3 migration
	if version == 2 {
		s.logger.Info("migrating database schema", "from", 2, "to", 3)
		if err := s.migrateV2toV3(ctx); err != nil {
			return fmt.Errorf("migration v2->v3: %w", err)
		}
		version = 3
	}

	// Handle version 3 -> 4 migration
	if version == 3 {
		s.logger.Info("migrating database schema", "from", 3, "to", 4)
		if err := s.migrateV3toV4(ctx); err != nil {
			return fmt.Errorf("migration v3->v4: %w", err)
		}
		version = 4
	}

	if version != 0 && version != schemaVersion {
		// Version mismatch - caller needs to delete and recreate
		return fmt.Errorf("schema version mismatch: have %d, want %d", version, schemaVersion)
	}

	// Execute the embedded schema (idempotent due to IF NOT EXISTS)
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("failed to execute schema: %w", err)
	}

	// Set schema version
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("failed to set schema version: %w", err)
	}

	return nil
}

// migrateV2toV3 migrates from schema version 2 to 3.
// This migration:
// - Adds metadata_json column to managed_programs
// - Migrates data from program_metadata_index to metadata_json
// - Drops program_tags and program_metadata_index tables
func (s *sqliteStore) migrateV2toV3(ctx context.Context) error {
	// Step 1: Add new column
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE managed_programs ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return fmt.Errorf("add metadata_json column: %w", err)
	}

	// Step 2: Migrate existing metadata from index table to JSON column
	if _, err := s.db.ExecContext(ctx, `
		UPDATE managed_programs SET metadata_json = COALESCE(
			(SELECT json_group_object(key, value)
			 FROM program_metadata_index
			 WHERE program_metadata_index.kernel_id = managed_programs.kernel_id),
			'{}'
		)
	`); err != nil {
		return fmt.Errorf("migrate metadata to JSON: %w", err)
	}

	// Step 3: Drop old tables
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS program_metadata_index`); err != nil {
		return fmt.Errorf("drop program_metadata_index: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS program_tags`); err != nil {
		return fmt.Errorf("drop program_tags: %w", err)
	}

	return nil
}

// migrateV3toV4 migrates from schema version 3 to 4.
// This migration adds the license column to managed_programs.
func (s *sqliteStore) migrateV3toV4(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE managed_programs ADD COLUMN license TEXT`); err != nil {
		return fmt.Errorf("add license column: %w", err)
	}
	return nil
}

// prepareStatements prepares all SQL statements for reuse.
func (s *sqliteStore) prepareStatements(ctx context.Context) error {
	if err := s.prepareProgramStatements(ctx); err != nil {
		return err
	}
	if err := s.prepareLinkRegistryStatements(ctx); err != nil {
		return err
	}
	if err := s.prepareLinkDetailStatements(ctx); err != nil {
		return err
	}
	return s.prepareDispatcherStatements(ctx)
}

// RunInTransaction executes the callback within a database transaction.
// If the callback returns nil, the transaction commits.
// If the callback returns an error, the transaction rolls back.
//
// # Prepared Statement Handling
//
// The Store holds "master" prepared statements that are compiled once when the
// database is opened and remain valid for the lifetime of the connection. These
// masters live on s.stmtXXX fields, prepared against *sql.DB.
//
// For transactional use, tx.StmtContext creates lightweight transaction-bound
// handles that reference the already-compiled master statements. No SQL parsing
// occurs here - we're just binding existing compiled queries to this transaction.
//
// After commit or rollback, the tx-bound handles become invalid, but that's fine:
// txStore goes out of scope and subsequent RunInTransaction calls create fresh
// handles from the still-valid masters. The masters are never invalidated by
// transaction lifecycle events.
func (s *sqliteStore) RunInTransaction(ctx context.Context, fn func(platform.Store) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	txStore := &sqliteStore{
		db:     s.db,
		conn:   tx,
		logger: s.logger,
		// Program statements
		stmtGetProgram:             tx.StmtContext(ctx, s.stmtGetProgram),
		stmtSaveProgram:            tx.StmtContext(ctx, s.stmtSaveProgram),
		stmtDeleteProgram:          tx.StmtContext(ctx, s.stmtDeleteProgram),
		stmtListPrograms:           tx.StmtContext(ctx, s.stmtListPrograms),
		stmtCountDependentPrograms: tx.StmtContext(ctx, s.stmtCountDependentPrograms),
		// Link registry statements
		stmtDeleteLink:              tx.StmtContext(ctx, s.stmtDeleteLink),
		stmtGetLinkRegistry:         tx.StmtContext(ctx, s.stmtGetLinkRegistry),
		stmtListLinks:               tx.StmtContext(ctx, s.stmtListLinks),
		stmtListLinksByProgram:      tx.StmtContext(ctx, s.stmtListLinksByProgram),
		stmtInsertLinkRegistry:      tx.StmtContext(ctx, s.stmtInsertLinkRegistry),
		stmtListTCXLinksByInterface: tx.StmtContext(ctx, s.stmtListTCXLinksByInterface),
		// Link detail get statements
		stmtGetTracepointDetails: tx.StmtContext(ctx, s.stmtGetTracepointDetails),
		stmtGetKprobeDetails:     tx.StmtContext(ctx, s.stmtGetKprobeDetails),
		stmtGetUprobeDetails:     tx.StmtContext(ctx, s.stmtGetUprobeDetails),
		stmtGetFentryDetails:     tx.StmtContext(ctx, s.stmtGetFentryDetails),
		stmtGetFexitDetails:      tx.StmtContext(ctx, s.stmtGetFexitDetails),
		stmtGetXDPDetails:        tx.StmtContext(ctx, s.stmtGetXDPDetails),
		stmtGetTCDetails:         tx.StmtContext(ctx, s.stmtGetTCDetails),
		stmtGetTCXDetails:        tx.StmtContext(ctx, s.stmtGetTCXDetails),
		// Link detail save statements
		stmtSaveTracepointDetails: tx.StmtContext(ctx, s.stmtSaveTracepointDetails),
		stmtSaveKprobeDetails:     tx.StmtContext(ctx, s.stmtSaveKprobeDetails),
		stmtSaveUprobeDetails:     tx.StmtContext(ctx, s.stmtSaveUprobeDetails),
		stmtSaveFentryDetails:     tx.StmtContext(ctx, s.stmtSaveFentryDetails),
		stmtSaveFexitDetails:      tx.StmtContext(ctx, s.stmtSaveFexitDetails),
		stmtSaveXDPDetails:        tx.StmtContext(ctx, s.stmtSaveXDPDetails),
		stmtSaveTCDetails:         tx.StmtContext(ctx, s.stmtSaveTCDetails),
		stmtSaveTCXDetails:        tx.StmtContext(ctx, s.stmtSaveTCXDetails),
		// Batch link detail list statements
		stmtListAllTracepointDetails: tx.StmtContext(ctx, s.stmtListAllTracepointDetails),
		stmtListAllKprobeDetails:     tx.StmtContext(ctx, s.stmtListAllKprobeDetails),
		stmtListAllUprobeDetails:     tx.StmtContext(ctx, s.stmtListAllUprobeDetails),
		stmtListAllFentryDetails:     tx.StmtContext(ctx, s.stmtListAllFentryDetails),
		stmtListAllFexitDetails:      tx.StmtContext(ctx, s.stmtListAllFexitDetails),
		stmtListAllXDPDetails:        tx.StmtContext(ctx, s.stmtListAllXDPDetails),
		stmtListAllTCDetails:         tx.StmtContext(ctx, s.stmtListAllTCDetails),
		stmtListAllTCXDetails:        tx.StmtContext(ctx, s.stmtListAllTCXDetails),
		// Dispatcher statements
		stmtGetDispatcher:        tx.StmtContext(ctx, s.stmtGetDispatcher),
		stmtListDispatchers:      tx.StmtContext(ctx, s.stmtListDispatchers),
		stmtSaveDispatcher:       tx.StmtContext(ctx, s.stmtSaveDispatcher),
		stmtDeleteDispatcher:     tx.StmtContext(ctx, s.stmtDeleteDispatcher),
		stmtIncrementRevision:    tx.StmtContext(ctx, s.stmtIncrementRevision),
		stmtGetDispatcherByType:  tx.StmtContext(ctx, s.stmtGetDispatcherByType),
		stmtCountDispatcherLinks: tx.StmtContext(ctx, s.stmtCountDispatcherLinks),
	}

	if err := fn(txStore); err != nil {
		return err
	}

	return tx.Commit()
}
