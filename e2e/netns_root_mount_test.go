//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// rootNetnsName is the name under /run/netns/ that we bind-mount
// the test process's own network namespace at, so command-line
// tools that take a netns name can target root via
// `ip netns exec root <cmd>`. Wrapping every netns-sensitive
// shell-out in `ip netns exec <ns>` makes the child explicitly
// setns into the named netns regardless of which Go OS thread
// performed the fork; that decouples test reliability from
// thread-state contamination upstream.
const rootNetnsName = "root"

// setupRootNetnsMount bind-mounts /proc/self/ns/net at
// /run/netns/<rootNetnsName>. Idempotent: a previous run that
// crashed before unmounting just gets re-bind-mounted in place.
// Returns an error rather than panicking so TestMain can decide
// how to react.
func setupRootNetnsMount() error {
	if err := os.MkdirAll("/run/netns", 0o755); err != nil {
		return fmt.Errorf("mkdir /run/netns: %w", err)
	}
	target := "/run/netns/" + rootNetnsName
	// Match iproute2's `ip netns add` behaviour: create the
	// target with mode 0 and O_RDONLY. SELinux on Fedora can
	// reject other modes for bind-mount targets under
	// /run/netns. Existing file from a previous (crashed) run
	// is fine -- we just bind-mount over it.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if err == nil {
		f.Close()
	}
	// If something is already mounted there, unmount first; we
	// can't tell from a stale empty file vs a stale bind-mount
	// without checking, so just attempt unmount and ignore the
	// "not mounted" error.
	_ = unix.Unmount(target, 0)
	if err := unix.Mount("/proc/self/ns/net", target, "none", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mount /proc/self/ns/net -> %s: %w", target, err)
	}
	return nil
}

// teardownRootNetnsMount unmounts and removes the bind-mount
// created by setupRootNetnsMount. Best-effort: any error is
// returned but TestMain may choose to log and continue.
func teardownRootNetnsMount() error {
	target := "/run/netns/" + rootNetnsName
	if err := unix.Unmount(target, 0); err != nil && !os.IsNotExist(err) {
		// Non-fatal; just continue to remove the file.
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", target, err)
	}
	return nil
}

// execInNetns runs the given command inside the named netns by
// shelling out to `ip netns exec <ns> <args...>`. The child
// process explicitly setns'es into /run/netns/<ns> before
// exec'ing the inner command, so the netns the child runs in
// is determined entirely by ns -- not by the calling Go
// thread's current netns. Use rootNetnsName for "in this
// process's root netns".
//
// Returns the combined stdout+stderr and any error from
// CombinedOutput.
func execInNetns(ns string, args ...string) ([]byte, error) {
	full := append([]string{"ip", "netns", "exec", ns}, args...)
	return exec.Command(full[0], full[1:]...).CombinedOutput()
}
