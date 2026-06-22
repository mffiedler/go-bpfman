// Package runtimestate opens bpfman's runtime filesystem, store, and
// kernel adapters.
package runtimestate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/frobware/go-bpfman/fs"
	fsruntime "github.com/frobware/go-bpfman/fs/runtime"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
)

// Mutable is bpfman's opened runtime state for callers that may
// mutate it.
type Mutable struct {
	FS         fs.Runtime
	Store      platform.Store
	Kernel     platform.KernelOperations
	Discoverer platform.ProgramDiscoverer
	Logger     *slog.Logger
}

// SnapshotReader reads a snapshot of bpfman's runtime state. It
// exposes observations, not store mutation.
type SnapshotReader struct {
	fs     fs.Runtime
	store  inspect.StoreLister
	kernel inspect.KernelLister
	close  func() error
}

// OpenMutable opens runtime state for callers that are allowed to
// create, migrate, or recreate the store while holding the writer lock.
func OpenMutable(ctx context.Context, layout fs.Layout, logger *slog.Logger, lockTimeout time.Duration) (*Mutable, error) {
	if !layout.Valid() {
		return nil, errors.New("invalid runtime layout")
	}
	logger = defaultLogger(logger)

	var store platform.Store
	err := lock.RunWithTimeout(ctx, layout.LockPath(), logger, lockTimeout, func(ctx context.Context, writeLock lock.WriterScope) error {
		var err error
		store, err = sqlite.New(ctx, layout.DBPath(), logger, writeLock)
		return err
	})
	if err != nil {
		return nil, err
	}

	opened, err := finishMutableOpen(layout, logger, store)
	if err != nil {
		store.Close()
		return nil, err
	}
	return opened, nil
}

// OpenSnapshotReader opens existing runtime state for inspection. It
// does not create, migrate, or recreate the store. It still ensures the
// runtime filesystem and bpffs mount are present so filesystem-backed
// observations can run.
func OpenSnapshotReader(ctx context.Context, layout fs.Layout, logger *slog.Logger) (*SnapshotReader, error) {
	if !layout.Valid() {
		return nil, errors.New("invalid runtime layout")
	}
	logger = defaultLogger(logger)

	store, err := sqlite.OpenExistingStore(ctx, layout.DBPath(), logger)
	if err != nil {
		return nil, err
	}
	reader, err := finishSnapshotReaderOpen(layout, logger, store)
	if err != nil {
		store.Close()
		return nil, err
	}
	return reader, nil
}

// Close releases runtime resources.
func (r *Mutable) Close() error {
	if r == nil || r.Store == nil {
		return nil
	}
	return r.Store.Close()
}

// Close releases runtime resources.
func (r *SnapshotReader) Close() error {
	if r == nil || r.close == nil {
		return nil
	}
	return r.close()
}

// Snapshot returns a correlated view of store, kernel, and bpffs state.
func (r *SnapshotReader) Snapshot(ctx context.Context) (*inspect.Observation, error) {
	return inspect.Snapshot(ctx, r.store, r.kernel, r.fs.BPFFS().Scanner())
}

func finishMutableOpen(layout fs.Layout, logger *slog.Logger, store platform.Store) (*Mutable, error) {
	ensuredFS, err := ensureFS(layout, logger)
	if err != nil {
		return nil, err
	}

	return &Mutable{
		FS:         ensuredFS,
		Store:      store,
		Kernel:     ebpf.New(ebpf.WithLogger(logger)),
		Discoverer: ebpf.NewProgramDiscoverer(),
		Logger:     logger,
	}, nil
}

func finishSnapshotReaderOpen(layout fs.Layout, logger *slog.Logger, store platform.Store) (*SnapshotReader, error) {
	ensuredFS, err := ensureFS(layout, logger)
	if err != nil {
		return nil, err
	}

	return &SnapshotReader{
		fs:     ensuredFS,
		store:  store,
		kernel: ebpf.New(ebpf.WithLogger(logger)),
		close:  store.Close,
	}, nil
}

func ensureFS(layout fs.Layout, logger *slog.Logger) (fs.Runtime, error) {
	ensuredFS, err := fsruntime.New(layout, fsruntime.RealMounter{}, logger)
	if err != nil {
		return fs.Runtime{}, fmt.Errorf("ensure runtime: %w", err)
	}
	return ensuredFS, nil
}

func defaultLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.Default()
}
