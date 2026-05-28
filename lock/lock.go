// Package lock provides a cross-process global writer lock using flock(2)
// to protect all mutations of /run/bpfman/... state.
//
// Design principle: "Illegal states unrepresentable" - use a non-forgeable
// scope token that proves the lock is held. Mutating operations require
// this token (compiler enforced). No context abuse.
//
// There are two ways to obtain proof of the lock:
//
//  1. Parents call Run(...) and receive a WriterScope capability.
//  2. Helpers receive a dup'd fd and call InheritedLockFromFD(...).
//
// Helpers never attempt to acquire the lock from a path; they only accept
// an inherited fd. This prevents subtle deadlocks and ensures namespace
// helpers cannot run without lock coverage.
package lock

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// WriterLockFDEnvVar is the environment variable used to pass the lock
// file descriptor to child processes (e.g., bpfman-ns helper).
const WriterLockFDEnvVar = "BPFMAN_WRITER_LOCK_FD"

// WriterScope represents the dynamic execution region in which the
// global bpfman writer lock is held.
//
// Possession of a WriterScope is proof that the caller holds exclusive
// write access to /run/bpfman/... state. WriterScope is a capability, not
// a mutex: it cannot be constructed, locked, or unlocked by callers.
//
// A WriterScope is only obtained by executing code under lock.Run(...).
// The interface cannot be implemented outside this package due to the
// unexported marker method.
type WriterScope interface {
	// DupFD duplicates the lock fd for passing to a child process.
	// The child inherits the lock via the duped fd.
	DupFD() (*os.File, error)

	// FD returns the raw lock file descriptor (for logging/diagnostics).
	FD() int

	// writerScopeMarker is unexported to prevent external implementations.
	writerScopeMarker()
}

// writerScope is the concrete implementation of WriterScope.
// It holds the exclusive flock and cannot be constructed outside this package.
type writerScope struct {
	f *os.File
}

func (*writerScope) writerScopeMarker() {}

func (s *writerScope) FD() int {
	return int(s.f.Fd())
}

func (s *writerScope) DupFD() (*os.File, error) {
	dup, err := syscall.Dup(s.FD())
	if err != nil {
		return nil, fmt.Errorf("dup lock fd: %w", err)
	}
	return os.NewFile(uintptr(dup), "bpfman-writer-lock"), nil
}

// Run acquires the global writer lock, executes fn, then releases.
// The WriterScope proves to callees that the lock is held.
// Uses LOCK_EX|LOCK_NB with exponential backoff, respects ctx cancellation.
func Run(ctx context.Context, lockPath string, fn func(context.Context, WriterScope) error) error {
	f, err := acquireWriter(ctx, lockPath)
	if err != nil {
		return err
	}
	defer f.Close()

	return fn(ctx, &writerScope{f: f})
}

// RunWithTiming wraps Run with timing logs for lock acquisition and release.
// The logger parameter is required; use Run directly if logging is not needed.
// Logs are tagged with component=lock for selective filtering.
func RunWithTiming(ctx context.Context, lockPath string, logger *slog.Logger, fn func(context.Context, WriterScope) error) error {
	logger = logger.With("component", "lock")
	start := time.Now()
	return Run(ctx, lockPath, func(ctx context.Context, scope WriterScope) error {
		acquired := time.Now()
		logger.DebugContext(ctx, "lock acquired", "path", lockPath, "wait_ms", acquired.Sub(start).Milliseconds())
		defer func() {
			logger.DebugContext(ctx, "lock released", "path", lockPath, "held_ms", time.Since(acquired).Milliseconds())
		}()
		return fn(ctx, scope)
	})
}

// acquireWriter opens the lock file and acquires exclusive lock.
// The parent directory is created on demand so first-touch
// invocations (no daemon has set up the runtime root yet) succeed
// in the same way O_CREATE handles the lock file itself.
func acquireWriter(ctx context.Context, path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("ensure lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	// Polling-style retry instead of a blocking flock so ctx
	// cancellation is honoured.  Start at 1ms (an uncontended lock
	// is free immediately, and a contended one usually clears in a
	// handful of milliseconds because the work-under-lock at the
	// other end is typically a few sqlite writes plus a kernel-side
	// op of similar order).  Double on every miss, capped at 500ms,
	// so deep queues do not spin hot but the common case sees
	// near-instant pickup as soon as the lock is released.  An
	// earlier 25ms initial value forced every contended waiter to
	// pay a full 25ms even when the holder released after 1-2ms,
	// making lock wait time the dominant factor in shared-runtime
	// e2e wall-clock once the per-mutation work itself was small.
	// The proper kernel-managed wait (F_OFD_SETLKW with signal-
	// based interrupt) is the longer-term fix and would remove the
	// polling entirely.
	backoff := 1 * time.Millisecond
	const maxBackoff = 500 * time.Millisecond

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}

		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// InheritedLock represents a writer lock inherited by a helper process.
// Unlike WriterScope (which is managed by Run), InheritedLock is closeable
// because the helper genuinely owns this fd for its lifetime.
type InheritedLock struct {
	f *os.File
}

// InheritedLockFromFD creates an InheritedLock from an already-held lock fd.
// Used by helper processes that receive the lock fd via ExtraFiles.
// Verifies the fd actually holds the lock.
//
// LIMITATION: flock(LOCK_EX|LOCK_NB) can only verify "I can hold EX now",
// not "parent definitely held it before passing". This is acceptable:
// - Parent MUST acquire before spawning (enforced by type system)
// - Helper must hold the lock regardless of how it got it
// - If parent didn't hold it, helper now does (still correct)
func InheritedLockFromFD(fd int) (*InheritedLock, error) {
	f := os.NewFile(uintptr(fd), "bpfman-writer-lock")
	if f == nil {
		return nil, fmt.Errorf("invalid fd %d", fd)
	}

	// Verify we can hold the lock exclusively.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("fd %d does not hold writer lock: %w", fd, err)
	}

	return &InheritedLock{f: f}, nil
}

// Close releases the lock. Called by helpers when done.
func (l *InheritedLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// FD returns the raw lock file descriptor (for logging/diagnostics).
func (l *InheritedLock) FD() int {
	return int(l.f.Fd())
}
