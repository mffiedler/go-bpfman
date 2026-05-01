// SPDX-License-Identifier: Apache-2.0

package netns

import (
	"fmt"
	"runtime"

	vishvananda "github.com/vishvananda/netns"
)

// CreateNamed creates a named netns at /run/netns/<name>.
//
// The calling goroutine is left in the netns it was in on
// entry. The function locks the calling OS thread internally
// to perform the kernel operations safely; on success the
// thread is unlocked before returning.
//
// FAILURE SEMANTICS:
//
// On any error after the new netns is created, the calling OS
// thread may be in a non-root netns. To prevent a poisoned
// thread from being returned to Go's scheduler -- where the
// next goroutine that lands on it would inherit the wrong
// netns identity, leading to subtle cross-goroutine state
// contamination -- this function does NOT unlock the OS thread
// on error. The caller's goroutine is still pinned to that
// thread when the error returns. The caller MUST exit the
// goroutine (via t.Fatalf, log.Fatal, runtime.Goexit, or
// panic) so Go's runtime destroys the thread instead of
// recycling it.
//
// If the caller ignores the error and returns normally, the
// thread will be returned to the scheduler in a non-root
// netns and will silently corrupt every goroutine that later
// runs on it -- including any code that reads
// /proc/self/ns/net to identify the "current" netns, which is
// per-thread.
//
// See runtime.LockOSThread for the runtime-level guarantee
// that an unmatched lock retires the thread on goroutine
// exit.
func CreateNamed(name string) error {
	runtime.LockOSThread()

	origNs, err := vishvananda.Get()
	if err != nil {
		// The thread did not move; safe to unlock.
		runtime.UnlockOSThread()
		return fmt.Errorf("get current netns: %w", err)
	}
	defer origNs.Close()

	newNs, err := vishvananda.NewNamed(name)
	if err != nil {
		// NewNamed may have already moved the thread into a
		// partially-created named netns. Do NOT unlock.
		return fmt.Errorf("create netns %s: %w", name, err)
	}
	newNs.Close()

	if err := vishvananda.Set(origNs); err != nil {
		// Restore failed; thread is still in the named netns.
		// Do NOT unlock.
		return fmt.Errorf("restore netns: %w", err)
	}

	// Restore succeeded; thread is back at the original netns.
	runtime.UnlockOSThread()
	return nil
}
