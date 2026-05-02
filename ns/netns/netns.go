// Package netns provides network namespace identification and switching functions.
package netns

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// GetCurrentNsid returns the inode number of the current network namespace.
// This inode uniquely identifies the network namespace and is used to
// construct dispatcher paths that match the Rust bpfman convention.
func GetCurrentNsid() (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/net", &stat); err != nil {
		return 0, fmt.Errorf("stat /proc/self/ns/net: %w", err)
	}
	return stat.Ino, nil
}

// processNsid is the inode number of the network namespace this
// process was started in, captured once at package init before
// any goroutine has had a chance to perturb thread state via
// setns/unshare. Used by GetNsid("") to return a stable,
// process-level value instead of reading per-thread
// /proc/self/ns/net (which is unsafe under heavy parallel netns
// activity: see ns/netns/named.go for the OS-thread
// contamination story). Captured at init() so the read happens
// from the still-pristine main thread.
var processNsid uint64

func init() {
	var stat syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/net", &stat); err != nil {
		panic(fmt.Errorf("netns: cannot stat /proc/self/ns/net at startup: %w", err))
	}
	processNsid = stat.Ino
}

// GetNsid returns the inode number of the network namespace at
// the given path. If path is empty, returns the netns the
// process was started in (captured once at init), NOT the
// calling thread's current netns. The latter is per-thread and
// can be poisoned by upstream library bugs in concurrent
// programs; reading the captured process value insulates the
// caller from that hazard. Callers wanting "this thread's
// current netns" must explicitly stat /proc/self/ns/net or
// /proc/<tid>/ns/net themselves.
func GetNsid(path string) (uint64, error) {
	if path == "" {
		return processNsid, nil
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return stat.Ino, nil
}

// Run executes fn in the network namespace specified by path.
// If path is empty, fn is executed in the current namespace (no switch).
// The original namespace is restored after fn returns, even if fn panics.
//
// FAILURE SEMANTICS:
//
// On the success path the calling OS thread is locked while fn
// runs and unlocked on return. If the deferred restore-to-the-
// original-netns fails (rare; would mean the kernel rejected
// setns back to the originally-open fd), the thread is in the
// target netns. To prevent that thread from being returned to
// Go's scheduler -- where the next goroutine that lands on it
// would inherit the wrong netns identity, silently corrupting
// any code that reads /proc/self/ns/net (which is per-thread)
// -- this function panics. The panic propagates out of Run,
// unwinds the goroutine, and the outer defer below skips
// runtime.UnlockOSThread (safeUnlock stays false). Go's runtime
// retires the thread on goroutine exit (see
// runtime.LockOSThread).
//
// Panic is the right escalation here because the alternative --
// silently returning while the goroutine remains pinned to a
// poisoned thread -- lets the goroutine continue doing work
// against the wrong netns until it happens to exit, leaking
// state corruption all the while. Loud failure beats quiet
// rot.
//
// In normal operation the restore succeeds, no panic fires,
// and the OS thread is unlocked cleanly.
//
// Usage:
//
//	err := netns.Run("/var/run/netns/target", func() error {
//	    // operations in target namespace
//	    return nil
//	})
func Run(path string, fn func() error) error {
	if path == "" {
		return fn()
	}

	runtime.LockOSThread()
	safeUnlock := false
	defer func() {
		if safeUnlock {
			runtime.UnlockOSThread()
		}
	}()

	// Open current namespace for restoration
	originalNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		safeUnlock = true // thread did not move
		return fmt.Errorf("open current netns: %w", err)
	}
	defer originalNS.Close()

	// Open target namespace
	targetNS, err := os.Open(path)
	if err != nil {
		safeUnlock = true // thread did not move
		return fmt.Errorf("open target netns %s: %w", path, err)
	}
	defer targetNS.Close()

	// Switch to target namespace
	if err := unix.Setns(int(targetNS.Fd()), unix.CLONE_NEWNET); err != nil {
		safeUnlock = true // setns failed; thread did not move
		return fmt.Errorf("setns to target netns: %w", err)
	}

	// Restore original namespace on return, even if fn panics.
	// On restore success, flip safeUnlock so the outer defer
	// runs UnlockOSThread. On restore failure, panic: the OS
	// thread is in a non-root netns and must not be returned
	// to the scheduler. The panic propagates out of Run,
	// unwinds the goroutine; safeUnlock stays false so the
	// outer defer skips the unlock; Go's runtime retires the
	// thread on goroutine exit.
	defer func() {
		if err := unix.Setns(int(originalNS.Fd()), unix.CLONE_NEWNET); err != nil {
			panic(fmt.Errorf("netns.Run: failed to restore original netns; OS thread is in target netns and cannot be safely returned to the scheduler: %w", err))
		}
		safeUnlock = true
	}()

	return fn()
}
