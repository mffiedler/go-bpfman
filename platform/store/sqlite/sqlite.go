package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/frobware/go-bpfman/platform"
)

// Tuning knob env vars. Both apply to every process that opens
// the store (the bpfman daemon AND every bpfman CLI invocation
// against the same DB), which keeps behaviour symmetric across
// the daemon/CLI split without each having its own config path.
// Set on the daemon's container env in Kubernetes; inherited by
// CLI invocations from inside the same container.
const (
	// envBusyTimeout overrides the SQLite busy_timeout pragma,
	// i.e. the wait budget for BeginTx(IMMEDIATE). Parsed as a
	// Go time.Duration ("5s", "30s", "500ms"). Default is
	// defaultBusyTimeout.
	envBusyTimeout = "BPFMAN_SQLITE_BUSY_TIMEOUT"
	// envTxRetryBackoffs overrides the Go-level retry schedule
	// applied on top of busy_timeout. Comma-separated durations,
	// e.g. "50ms,200ms,800ms". An empty value (the env var is
	// set but has no value) disables retry entirely. Unset uses
	// defaultTxRetryBackoffs.
	envTxRetryBackoffs = "BPFMAN_SQLITE_TX_RETRY_BACKOFFS"

	defaultBusyTimeout = 5 * time.Second
)

// defaultTxRetryBackoffs is the bounded exponential-backoff
// schedule applied on top of SQLite's own busy_timeout. Three
// extra attempts after the initial try, ~1.05s of sleep across
// the three pauses; combined with busy_timeout the worst-case
// per-tx latency is roughly busy_timeout * 4 + 1.05s.
var defaultTxRetryBackoffs = []time.Duration{
	50 * time.Millisecond,
	200 * time.Millisecond,
	800 * time.Millisecond,
}

// resolveTuning consults the env vars and returns the effective
// busy_timeout / retry backoffs for one store. Invalid values
// are logged at WARN and the default is used instead -- starting
// with degraded settings beats refusing to open the store.
func resolveTuning(logger *slog.Logger) (time.Duration, []time.Duration) {
	busy := defaultBusyTimeout
	if s, ok := os.LookupEnv(envBusyTimeout); ok {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			busy = d
		} else {
			logger.Warn("invalid env, using default",
				"env", envBusyTimeout, "value", s, "default", defaultBusyTimeout)
		}
	}

	backoffs := defaultTxRetryBackoffs
	if s, ok := os.LookupEnv(envTxRetryBackoffs); ok {
		parsed, err := parseDurationList(s)
		if err != nil {
			logger.Warn("invalid env, using default",
				"env", envTxRetryBackoffs, "value", s, "error", err,
				"default", defaultTxRetryBackoffs)
		} else {
			backoffs = parsed
		}
	}
	return busy, backoffs
}

// parseDurationList parses a comma-separated list of Go
// durations. An empty (but set) string returns an empty
// (non-nil) slice, which the retry loop treats as "disabled".
// Whitespace around each entry is tolerated so the env value
// reads cleanly in YAML, where "50ms, 200ms" is more natural
// than the no-space form.
func parseDurationList(s string) ([]time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []time.Duration{}, nil
	}
	parts := strings.Split(s, ",")
	out := make([]time.Duration, len(parts))
	for i, p := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("entry %d %q: %w", i, p, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("entry %d %q: negative duration", i, p)
		}
		out[i] = d
	}
	return out, nil
}

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

	// txRetryBackoffs is the Go-level retry schedule for
	// SQLITE_BUSY surfaced past SQLite's own busy_timeout. Pinned
	// to the store at New time (resolved from
	// BPFMAN_SQLITE_TX_RETRY_BACKOFFS or the package default) so
	// every transaction shares one consistent schedule and a
	// per-process override does not race a package-level var.
	txRetryBackoffs []time.Duration

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

	// Prepared statements for shared map pin operations
	stmtSaveSharedMapPin         *sql.Stmt
	stmtDeleteSharedMapPins      *sql.Stmt
	stmtListSharedMapsByProgram  *sql.Stmt
	stmtCountSharedMapRefs       *sql.Stmt
	stmtListReferencedSharedMaps *sql.Stmt

	// Prepared statements for dispatcher operations
	stmtGetDispatcher           *sql.Stmt
	stmtGetXDPMembers           *sql.Stmt
	stmtGetTCMembers            *sql.Stmt
	stmtListDispatcherSummaries *sql.Stmt
	stmtDeleteXDPExtLinks       *sql.Stmt
	stmtDeleteTCExtLinks        *sql.Stmt
	stmtUpsertDispatcher        *sql.Stmt
	stmtUpsertExtLink           *sql.Stmt
	stmtInsertXDPDetail         *sql.Stmt
	stmtInsertTCDetail          *sql.Stmt
	stmtDeleteDispatcher        *sql.Stmt
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

	// _txlock=immediate makes BeginTx acquire the writer lock at
	// transaction start. Without it (the database/sql default of
	// BEGIN DEFERRED) two transactions that both read then write
	// can deadlock at the read-to-write upgrade; sqlite breaks the
	// deadlock by returning SQLITE_BUSY_SNAPSHOT immediately,
	// bypassing busy_timeout. With IMMEDIATE the wait happens at
	// BeginTx where busy_timeout applies cleanly.
	//
	// busy_timeout gives sqlite's internal busy handler a wait
	// budget per BeginTx before it surfaces SQLITE_BUSY to the
	// caller. RunInTransaction wraps each call in a Go-level
	// retry loop (see txRetryBackoffs on the store) that catches
	// any SQLITE_BUSY the inner budget could not absorb. Both
	// knobs are tunable through BPFMAN_SQLITE_BUSY_TIMEOUT and
	// BPFMAN_SQLITE_TX_RETRY_BACKOFFS; the worst-case latency
	// for a single transaction is bounded by busy_timeout *
	// (len(txRetryBackoffs)+1) plus the sum of the outer pauses.
	//
	// The two layers stack rather than compete: the inner
	// budget handles short bursts where the writer lock is
	// released within a few seconds; the outer retry handles
	// the long-tail outlier where a single slow flock holder
	// in the bpfman daemon pins the writer for longer.
	busyTimeout, txRetryBackoffs := resolveTuning(logger)
	db, err := sql.Open(driverName, dsn(dbPath, [][2]string{
		{"journal_mode", "WAL"},
		{"synchronous", "NORMAL"},
		{"foreign_keys", "1"},
		{"busy_timeout", strconv.FormatInt(busyTimeout.Milliseconds(), 10)},
	})+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	s := &sqliteStore{db: db, conn: db, logger: logger, txRetryBackoffs: txRetryBackoffs}
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

	logger.Info("opened database",
		"path", dbPath,
		"busy_timeout", busyTimeout,
		"tx_retry_backoffs", txRetryBackoffs)
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

	// In-memory stores don't observe writer contention from
	// other processes (no shared file) but still honour the
	// env-driven retry budget so tests can exercise the retry
	// code path under the same configuration shape.
	_, txRetryBackoffs := resolveTuning(logger)
	s := &sqliteStore{db: db, conn: db, logger: logger, txRetryBackoffs: txRetryBackoffs}
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
		s.stmtSaveSharedMapPin,
		s.stmtDeleteSharedMapPins,
		s.stmtListSharedMapsByProgram,
		s.stmtCountSharedMapRefs,
		s.stmtListReferencedSharedMaps,
		s.stmtGetDispatcher,
		s.stmtGetXDPMembers,
		s.stmtGetTCMembers,
		s.stmtListDispatcherSummaries,
		s.stmtDeleteXDPExtLinks,
		s.stmtDeleteTCExtLinks,
		s.stmtUpsertDispatcher,
		s.stmtUpsertExtLink,
		s.stmtInsertXDPDetail,
		s.stmtInsertTCDetail,
		s.stmtDeleteDispatcher,
	}
	for _, stmt := range stmts {
		if stmt != nil {
			stmt.Close()
		}
	}
}

// schemaVersion is the current schema version. Increment this when the schema changes.
// Migrations are supported from version 2 onwards.
const schemaVersion = 10

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
	if err := s.prepareSharedMapPinStatements(ctx); err != nil {
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
func (s *sqliteStore) RunInTransaction(ctx context.Context, name string, fn func(platform.Store) error) error {
	// Timing instrumentation mirrors lock.RunWithTiming's shape.
	// wait_ms is how long BeginTx blocked waiting for sqlite's
	// writer lock; with _txlock=immediate the IMMEDIATE acquire
	// happens up-front so this is the queue depth indicator.
	// held_ms is how long the transaction was open from BeginTx
	// to Commit/Rollback, including the caller's fn body.
	// The tx field carries the caller-supplied classifier so log
	// queries can group by transaction kind. Tagged
	// component=store; enable with BPFMAN_LOG=info,store=debug.
	//
	// SQLITE_BUSY handling: the call is retried with bounded
	// exponential backoff up to len(txRetryBackoffs) extra
	// attempts. The caller's fn is re-run from scratch on each
	// attempt, so fn must be idempotent against transactions
	// that succeeded part-way and rolled back -- every
	// RunInTransaction site in this tree is (snapshot-based
	// dispatcher rebuilds, idempotent inserts, deletes).
	logger := s.logger.With("component", "store", "tx", name)
	for attempt := 0; ; attempt++ {
		err := s.runTransactionAttempt(ctx, logger, attempt, fn)
		if err == nil {
			if attempt > 0 {
				// We entered the outer retry loop and
				// eventually succeeded. Surface the
				// recovery (and how deep we had to go)
				// at INFO so the operator can correlate
				// it to the WARN "tx busy, retrying"
				// records and gauge how often retries
				// actually save the caller from a
				// surfaced SQLITE_BUSY.
				logger.InfoContext(ctx, "tx recovered after retry",
					"attempts", attempt+1)
			}
			return nil
		}
		if !isBusyError(err) || attempt >= len(s.txRetryBackoffs) {
			return err
		}
		wait := s.txRetryBackoffs[attempt]
		logger.WarnContext(ctx, "tx busy, retrying",
			"attempt", attempt+1, "max_attempts", len(s.txRetryBackoffs)+1,
			"backoff_ms", wait.Milliseconds(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (s *sqliteStore) runTransactionAttempt(ctx context.Context, logger *slog.Logger, attempt int, fn func(platform.Store) error) error {
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		logger.DebugContext(ctx, "tx begin failed",
			"attempt", attempt, "wait_ms", time.Since(start).Milliseconds(), "error", err)
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	acquired := time.Now()
	logger.DebugContext(ctx, "tx acquired",
		"attempt", attempt, "wait_ms", acquired.Sub(start).Milliseconds())
	defer func() {
		logger.DebugContext(ctx, "tx closed",
			"attempt", attempt, "held_ms", time.Since(acquired).Milliseconds())
	}()
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
		// Shared map pin statements
		stmtSaveSharedMapPin:         tx.StmtContext(ctx, s.stmtSaveSharedMapPin),
		stmtDeleteSharedMapPins:      tx.StmtContext(ctx, s.stmtDeleteSharedMapPins),
		stmtListSharedMapsByProgram:  tx.StmtContext(ctx, s.stmtListSharedMapsByProgram),
		stmtCountSharedMapRefs:       tx.StmtContext(ctx, s.stmtCountSharedMapRefs),
		stmtListReferencedSharedMaps: tx.StmtContext(ctx, s.stmtListReferencedSharedMaps),
		// Dispatcher statements
		stmtGetDispatcher:           tx.StmtContext(ctx, s.stmtGetDispatcher),
		stmtGetXDPMembers:           tx.StmtContext(ctx, s.stmtGetXDPMembers),
		stmtGetTCMembers:            tx.StmtContext(ctx, s.stmtGetTCMembers),
		stmtListDispatcherSummaries: tx.StmtContext(ctx, s.stmtListDispatcherSummaries),
		stmtDeleteXDPExtLinks:       tx.StmtContext(ctx, s.stmtDeleteXDPExtLinks),
		stmtDeleteTCExtLinks:        tx.StmtContext(ctx, s.stmtDeleteTCExtLinks),
		stmtUpsertDispatcher:        tx.StmtContext(ctx, s.stmtUpsertDispatcher),
		stmtUpsertExtLink:           tx.StmtContext(ctx, s.stmtUpsertExtLink),
		stmtInsertXDPDetail:         tx.StmtContext(ctx, s.stmtInsertXDPDetail),
		stmtInsertTCDetail:          tx.StmtContext(ctx, s.stmtInsertTCDetail),
		stmtDeleteDispatcher:        tx.StmtContext(ctx, s.stmtDeleteDispatcher),
	}

	if err := fn(txStore); err != nil {
		return err
	}

	return tx.Commit()
}
