// Test-fixture mode for the e2e/new translations that need a
// stable-PID worker firing kill(2) syscalls. Each kill(self_pid,
// SIGUSR1) fires the syscalls/sys_enter_kill tracepoint exactly
// once. SIGUSR1's default disposition would terminate the worker;
// the handler is replaced with a no-op before any kills go out.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// runKillFireWorker is the entry point for
// BPFMAN_SHELL_MODE=kill-fire-worker. Args:
//
//	SENTINEL_PREFIX ACK_PREFIX N K
//
// For each wave w in 1..K, blocks until SENTINEL_PREFIX.w
// exists, sends SIGUSR1 to itself N times, then creates
// ACK_PREFIX.w.
func runKillFireWorker(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("kill-fire-worker: usage: SENTINEL_PREFIX ACK_PREFIX N K (got %d args)", len(args))
	}
	sentinelPrefix := args[0]
	ackPrefix := args[1]
	n, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("kill-fire-worker: invalid N %q: %w", args[2], err)
	}
	k, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("kill-fire-worker: invalid K %q: %w", args[3], err)
	}

	// signal.Notify with a handler that drains the channel keeps
	// the Go runtime from terminating on SIGUSR1 while still
	// letting the kernel record the kill(2) entry that fires the
	// tracepoint.
	sigCh := make(chan os.Signal, 1024)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
		}
	}()

	pid := os.Getpid()
	for wave := 1; wave <= k; wave++ {
		sentinel := fmt.Sprintf("%s.%d", sentinelPrefix, wave)
		ack := fmt.Sprintf("%s.%d", ackPrefix, wave)
		for {
			if _, err := os.Stat(sentinel); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		for i := 0; i < n; i++ {
			if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
				return fmt.Errorf("kill-fire-worker: kill wave=%d i=%d: %w", wave, i, err)
			}
		}
		f, err := os.OpenFile(ack, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("kill-fire-worker: create ack %s: %w", ack, err)
		}
		f.Close()
	}
	return nil
}
