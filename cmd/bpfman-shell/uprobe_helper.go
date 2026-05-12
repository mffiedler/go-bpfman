// Test-fixture mode for the uprobe family of e2e/new translations.
// When BPFMAN_SHELL_MODE=uprobe-fire-worker, bpfman-shell runs as
// a stable-PID worker that fires bpfman_shell_uprobe_call_malloc
// N times per wave, gated by numbered sentinel/ack files.
//
// The cgo'd target symbol is declared with noinline + optimize(0)
// so the compiler cannot reduce the body to nothing and inline
// every caller, and the body has an unelidable side effect
// (malloc + free) so there are real instructions for the kernel
// uprobe to fire on. The DSL script attaches uprobes to the same
// bpfman-shell binary at this symbol, then drives the wave
// protocol via the sentinel/ack files.
//
// One binary, multiple modes, with the fixture co-located with
// the runner so there is no separate helper binary on disk and
// no dependency on locating libc paths (which break on NixOS,
// Guix, musl, and other non-standard layouts).

package main

// #include <stdlib.h>
// __attribute__((noinline, optimize("O0")))
// void bpfman_shell_uprobe_call_malloc(void) {
//     volatile void *p = malloc(1);
//     free((void *)p);
// }
import "C"

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func init() {
	registerFireKind("uprobe", fireKind{
		Mode:        "uprobe-fire-worker",
		Summary:     "Fire uprobe target symbol bpfman_shell_uprobe_call_malloc.",
		NeedsBinary: true,
	})
}

// invokeUprobeCallMalloc calls the cgo'd target symbol once,
// firing whichever uprobe (or uretprobe) is attached to it.
func invokeUprobeCallMalloc() {
	C.bpfman_shell_uprobe_call_malloc()
}

// runUprobeFireWorker is the entry point for
// BPFMAN_SHELL_MODE=uprobe-fire-worker. Args:
//
//	SENTINEL_PREFIX ACK_PREFIX N K
//
// For each wave w in 1..K, blocks until SENTINEL_PREFIX.w
// exists, fires the target symbol N times, then creates
// ACK_PREFIX.w. Exits 0 after wave K's ack.
//
// The script side removes sentinels and acks under bpfman-shell's
// own (non-worker) PID, so those file operations do not register
// against the BPF program's expected_pid filter.
func runUprobeFireWorker(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("uprobe-fire-worker: usage: SENTINEL_PREFIX ACK_PREFIX N K (got %d args)", len(args))
	}
	sentinelPrefix := args[0]
	ackPrefix := args[1]
	n, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("uprobe-fire-worker: invalid N %q: %w", args[2], err)
	}
	k, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("uprobe-fire-worker: invalid K %q: %w", args[3], err)
	}

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
			invokeUprobeCallMalloc()
		}
		f, err := os.OpenFile(ack, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("uprobe-fire-worker: create ack %s: %w", ack, err)
		}
		f.Close()
	}
	return nil
}
