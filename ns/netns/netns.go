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

// GetNsid returns the inode number of the network namespace at the given path.
// If path is empty, returns the current namespace's inode.
func GetNsid(path string) (uint64, error) {
	if path == "" {
		return GetCurrentNsid()
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return stat.Ino, nil
}

// Run executes fn in the network namespace specified by path.
// If path is empty, fn is executed in the current namespace (no switch).
// The original namespace is always restored after fn returns, even if fn panics.
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
	defer runtime.UnlockOSThread()

	// Open current namespace for restoration
	originalNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open current netns: %w", err)
	}
	defer originalNS.Close()

	// Open target namespace
	targetNS, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open target netns %s: %w", path, err)
	}
	defer targetNS.Close()

	// Switch to target namespace
	if err := unix.Setns(int(targetNS.Fd()), unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("setns to target netns: %w", err)
	}

	// Ensure we restore original namespace even on panic
	defer func() {
		// Ignore error - we're in cleanup, and the thread will be
		// destroyed anyway if we can't restore
		_ = unix.Setns(int(originalNS.Fd()), unix.CLONE_NEWNET)
	}()

	return fn()
}
