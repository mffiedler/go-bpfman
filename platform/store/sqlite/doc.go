// Package sqlite implements [platform.Store] using SQLite.
//
// # Overview
//
// This package provides the concrete database layer for bpfman's
// program, link, and dispatcher metadata. It implements all the
// narrow store interfaces defined in platform/ (ProgramReader,
// ProgramWriter, LinkWriter, DispatcherStore, GarbageCollector, etc.)
// against a single SQLite database.
//
// # Schema
//
// The schema (schema.sql) uses a polymorphic registry pattern for
// links: a links table with a kind discriminator column and separate
// detail tables per link type (link_tracepoint_details,
// link_kprobe_details, link_xdp_details, link_tc_details,
// link_tcx_details, link_uprobe_details, link_fentry_details,
// link_fexit_details). Programs store user metadata as a JSON column
// (metadata_json).
//
// # Driver Selection
//
// Two SQLite drivers are supported, selected at build time:
//
//   - modernc.org/sqlite (default): pure Go, no CGO required for the
//     database layer (though the application still requires
//     CGO_ENABLED=1 for the nsenter package)
//   - mattn/go-sqlite3: CGO-based, selected with -tags cgo_sqlite
//
// # Calling Conventions
//
// This store is a pure data access layer with no internal transaction
// management. Individual methods execute against s.conn, which may be
// either the underlying *sql.DB (autocommit mode) or a *sql.Tx
// (transactional mode).
//
// For operations that require atomicity across multiple calls, use
// RunInTransaction:
//
//	err := store.RunInTransaction(ctx, func(txStore platform.Store) error {
//	    if err := txStore.Save(ctx, id, prog); err != nil {
//	        return err // triggers rollback
//	    }
//	    return txStore.SaveLink(ctx, record) // commits if nil
//	})
//
// # Autocommit Behaviour
//
// When methods are called outside a transaction (directly on the
// store), each SQL statement executes in its own implicit transaction
// that commits immediately upon completion. This means:
//
//   - Single-statement methods (Get, Delete, List) are atomic by
//     themselves.
//   - Multi-statement methods (Save, SaveLink) are NOT atomic: if the
//     second statement fails, the first statement's changes are
//     already committed. For example, Save inserts the program, then
//     deletes old metadata index entries, then inserts new ones. A
//     failure partway through leaves partial state.
//
// # WAL Mode and Reader/Writer Implications
//
// The database is opened with WAL (Write-Ahead Logging) mode, which
// provides:
//
//   - Readers do not block writers; writers do not block readers.
//   - A reader sees a consistent snapshot from when its transaction
//     (or statement in autocommit mode) began.
//   - Without an explicit transaction, consecutive reads may see
//     changes from concurrent writers between reads. Use
//     RunInTransaction for consistent multi-read operations.
//
// WAL mode is enabled for better crash recovery and write
// performance, though its concurrency benefits are secondary given
// the application-level RWMutex already coordinates access.
//
// # When to Use RunInTransaction
//
// Use RunInTransaction when you need:
//
//   - Atomicity: all-or-nothing semantics across multiple operations
//   - Consistency: read-your-writes within a sequence of operations
//   - Isolation: a stable view of data across multiple reads
//
// The caller (typically the manager or executor layer) decides
// atomicity requirements based on the operation being performed.
//
// # Concurrency Model
//
// The application layer (manager) serialises access via an RWMutex:
// multiple readers can proceed concurrently, but writers get exclusive
// access. This means there is no concurrent writer contention at the
// database level.
//
// SQLite transactions use the default DEFERRED type, which is
// sufficient given the application-level serialisation. The
// transaction provides atomicity and rollback semantics, not
// concurrent writer coordination.
//
// # SQLite Transaction Types
//
// SQLite supports three transaction types, specified at BEGIN:
//
//   - DEFERRED (default): no locks are acquired until the first read
//     or write. A read acquires a SHARED lock (allowing other
//     readers). A write acquires a RESERVED lock (blocking other
//     writers but allowing readers), then an EXCLUSIVE lock at commit
//     time. Risk: a read-then-write transaction may fail at write time
//     if another connection acquired a write lock in between.
//
//   - IMMEDIATE: acquires a RESERVED lock immediately when the
//     transaction begins, blocking other writers but allowing readers.
//     Guarantees that writes will succeed (no "database is locked"
//     errors mid-transaction). Preferred for transactions that will
//     write, but Go's database/sql does not expose this directly.
//
//   - EXCLUSIVE: acquires an EXCLUSIVE lock immediately, blocking all
//     other connections (readers and writers). Rarely needed; mainly
//     useful when you need to guarantee no other connection accesses
//     the database at all.
//
// This implementation uses DEFERRED because: (1) Go's database/sql
// does not expose SQLite-specific transaction types, and (2) the
// application-level RWMutex already prevents concurrent writers,
// making IMMEDIATE unnecessary.
//
// # Prepared Statements
//
// All SQL queries use prepared statements rather than inline SQL
// strings. When a query is executed with an inline string (e.g.,
// db.QueryContext(ctx, "SELECT ...")), SQLite must parse the SQL
// text, validate it, and generate a query plan on every call.
// Prepared statements move this work to initialisation time: the SQL
// is parsed and compiled once, and subsequent executions reuse the
// compiled representation.
//
// Benefits:
//
//   - Reduced CPU overhead: parsing and planning happen once, not
//     per-query
//   - Predictable latency: no parsing jitter during normal operations
//   - Cleaner code: SQL is defined in one place (prepareStatements)
//     rather than scattered across methods
//
// The cost is modest additional complexity in managing statement
// lifecycles, particularly for transactions where tx.StmtContext must
// create transaction-bound handles from the master statements. See
// RunInTransaction for details.
//
// # Garbage Collection
//
// The GC method removes all stored entries (programs, dispatchers,
// links) whose kernel IDs are absent from the provided live sets. It
// handles ordering constraints: extension links are removed before
// their dispatcher programs, and dependent programs are removed
// before map owners.
package sqlite
