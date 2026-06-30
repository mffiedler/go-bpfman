// Package runtime holds command-line helpers for binaries that open
// bpfman runtime state.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bpfman/bpfman/cmd/internal/cli"
	"github.com/bpfman/bpfman/fs"
	"github.com/bpfman/bpfman/lock"
)

// CLI extends the shared command CLI with bpfman runtime flags.
type CLI struct {
	cli.CLI

	// RuntimeDir is the root directory for bpfman runtime files (--runtime-dir), used to derive the filesystem layout and the writer-lock path. It defaults to the standard runtime root (/run/bpfman).
	RuntimeDir string `name:"runtime-dir" placeholder:"DIR" group:"global" help:"Root directory for runtime files." default:"${default_runtime_dir}"`

	// ImageCacheDir is the root directory for the OCI image cache (--image-cache-dir). It defaults to /var/cache/bpfman.
	ImageCacheDir string `name:"image-cache-dir" placeholder:"DIR" group:"global" help:"Root directory for OCI image cache." default:"${default_image_cache_dir}"`

	// LockTimeout bounds how long to wait when acquiring the global writer lock (--lock-timeout or BPFMAN_LOCK_TIMEOUT); zero waits indefinitely. It defaults to 30s.
	LockTimeout time.Duration `name:"lock-timeout" placeholder:"DURATION" group:"global" help:"Timeout for acquiring the global writer lock (0 for indefinite)." default:"30s" env:"BPFMAN_LOCK_TIMEOUT"`
}

// Layout returns the filesystem layout for the configured runtime directory.
func (c *CLI) Layout() (fs.Layout, error) {
	return fs.New(c.RuntimeDir)
}

// ImageCache returns the image cache for the configured cache directory.
func (c *CLI) ImageCache() (fs.ImageCache, error) {
	return fs.NewImageCache(c.ImageCacheDir)
}

// EnsuredImageCache returns an EnsuredImageCache capability token,
// creating the cache directory if needed.
func (c *CLI) EnsuredImageCache() (fs.EnsuredImageCache, error) {
	cache, err := c.ImageCache()
	if err != nil {
		return fs.EnsuredImageCache{}, err
	}
	return fs.EnsureCache(cache)
}

// RunWithLock wraps mutating operations with the global writer lock.
// The lock ensures serialised access to BPF state across concurrent
// CLI invocations.
func (c *CLI) RunWithLock(ctx context.Context, fn func(context.Context, lock.WriterScope) error) error {
	return RunWithLock(ctx, c, fn)
}

// RunWithLock executes fn under the global writer lock.
func RunWithLock(ctx context.Context, c *CLI, fn func(context.Context, lock.WriterScope) error) error {
	_, err := RunWithLockValue(ctx, c, func(ctx context.Context, writeLock lock.WriterScope) (struct{}, error) {
		return struct{}{}, fn(ctx, writeLock)
	})
	return err
}

// RunWithLockValue is like RunWithLock but returns a value from the
// locked section. Use this pattern to perform mutations under lock
// then format and emit output outside the lock to keep the critical
// section short.
func RunWithLockValue[T any](ctx context.Context, c *CLI, fn func(context.Context, lock.WriterScope) (T, error)) (T, error) {
	var result T

	layout, err := c.Layout()
	if err != nil {
		return result, fmt.Errorf("invalid runtime directory: %w", err)
	}

	logger := c.Logger()
	if logger == nil {
		logger = slog.Default()
	}
	err = lock.RunWithTimeout(ctx, layout.LockPath(), logger, c.LockTimeout, func(ctx context.Context, writeLock lock.WriterScope) error {
		var fnErr error
		result, fnErr = fn(ctx, writeLock)
		return fnErr
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

// RunBatchMutation executes mutate for each ID under the global
// writer lock, collects errors, and prints failures after releasing
// the lock. Returns a summary error if any mutations failed.
func RunBatchMutation[ID ~uint32 | ~uint64](
	ctx context.Context,
	c *CLI,
	ids []ID,
	noun string,
	verb string,
	mutate func(context.Context, lock.WriterScope, ID) error,
) error {
	type result struct {
		id  ID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := RunWithLock(ctx, c, func(ctx context.Context, writeLock lock.WriterScope) error {
		for _, id := range ids {
			err := mutate(ctx, writeLock, id)
			results = append(results, result{id: id, err: err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = c.PrintErrf("%s %d: %v\n", noun, r.id, r.err)
			failCount++
		}
	}
	if failCount > 0 {
		return fmt.Errorf("%d of %d %s(s) failed to %s", failCount, len(results), noun, verb)
	}
	return nil
}
