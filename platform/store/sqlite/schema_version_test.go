package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/lock"
)

// TestSchemaVersionMismatchRecreatesDatabase locks in the wipe-and-recreate
// contract that the links.metadata_json column relies on: rather than
// migrating, a bumped schemaVersion makes New delete and recreate an older
// database. It proves the version path (rewinding user_version triggers a
// clean recreate at the current version); it does not construct a
// column-less v13 database, because wipe-on-mismatch -- not in-place
// migration -- is the intended contract.
func TestSchemaVersionMismatchRecreatesDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	lockPath := filepath.Join(dir, ".lock")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// Create a current-version database, then close it.
	require.NoError(t, lock.Run(ctx, lockPath, func(ctx context.Context, wl lock.WriterScope) error {
		store, err := New(ctx, dbPath, logger, wl)
		if err != nil {
			return err
		}

		defer store.Close()
		return nil
	}))

	// Rewind the on-disk schema version to simulate a database written by an
	// earlier build, before links.metadata_json existed.
	raw, err := sql.Open(driverName, dbPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion-1))
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Reopening must delete and recreate against the current schema rather
	// than erroring on the version (and column) mismatch.
	require.NoError(t, lock.Run(ctx, lockPath, func(ctx context.Context, wl lock.WriterScope) error {
		store, err := New(ctx, dbPath, logger, wl)
		if err != nil {
			return err
		}

		defer store.Close()
		return nil
	}))

	// The recreated database must carry the current schema version.
	check, err := sql.Open(driverName, dbPath)
	require.NoError(t, err)
	defer check.Close()
	var version int
	require.NoError(t, check.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	require.Equal(t, schemaVersion, version, "recreated database should be at the current schema version")
}
