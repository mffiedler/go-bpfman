// Test-fixture mode for the e2e/new translations that need a
// stable-PID worker firing unlinkat(2) syscalls. Unlinkat fires
// both:
//   - sys_enter_unlinkat tracepoint (and sys_exit_unlinkat)
//   - do_unlinkat kernel function (kprobe / kretprobe / fentry / fexit)
//
// Calling unlinkat directly via golang.org/x/sys/unix gives
// deterministic syscall choice independent of host glibc and
// Go-runtime version. Tests with a tracepoint on
// sys_enter_unlinkat (like the multi-prog mixed test) need
// unlinkat semantics specifically; tests with a hook on
// do_unlinkat are happy with either, but standardising on
// unlinkat keeps the worker uniform across the family.

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	registerFireKind("unlinkat", fireKind{
		Mode:        "unlinkat-fire-worker",
		Summary:     "Fire unlinkat(2) syscalls for do_unlinkat / sys_*_unlinkat hooks.",
		NeedsBinary: false,
	})
}

// runUnlinkatFireWorker is the entry point for
// BPFMAN_SHELL_MODE=unlinkat-fire-worker. Args:
//
//	SENTINEL_PREFIX ACK_PREFIX N K
//
// For each wave w in 1..K, blocks until SENTINEL_PREFIX.w
// exists, fires unlinkat N times, then creates ACK_PREFIX.w.
// Each fire is "create file, unlinkat the file"; absolute paths
// avoid any directory-management syscalls (Go's Rmdir is
// implemented as unlinkat(AT_FDCWD, ..., AT_REMOVEDIR) on Linux,
// which would inflate the test's counter by one per cleanup).
func runUnlinkatFireWorker(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("unlinkat-fire-worker: usage: SENTINEL_PREFIX ACK_PREFIX N K (got %d args)", len(args))
	}
	sentinelPrefix := args[0]
	ackPrefix := args[1]
	n, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("unlinkat-fire-worker: invalid N %q: %w", args[2], err)
	}
	k, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("unlinkat-fire-worker: invalid K %q: %w", args[3], err)
	}

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
			path := fmt.Sprintf("/tmp/bpfman-shell-uat-%d-%d-%d", pid, wave, i)
			fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_TRUNC, 0o644)
			if err != nil {
				return fmt.Errorf("unlinkat-fire-worker: open wave=%d i=%d: %w", wave, i, err)
			}
			syscall.Close(fd)
			if err := unix.Unlinkat(unix.AT_FDCWD, path, 0); err != nil {
				return fmt.Errorf("unlinkat-fire-worker: unlinkat wave=%d i=%d: %w", wave, i, err)
			}
		}
		f, err := os.OpenFile(ack, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("unlinkat-fire-worker: create ack %s: %w", ack, err)
		}
		f.Close()
	}
	return nil
}
