// Package nsenter provides mount namespace switching for uprobe attachment
// in containers.
//
// This package uses a CGO constructor that runs before the Go runtime starts,
// allowing setns(CLONE_NEWNS) to work (which requires a single-threaded process).
//
// The approach is inspired by runc's libcontainer/nsenter but simplified for
// bpfman's uprobe use case.
//
// # How it works
//
// 1. When any binary that imports this package starts, the C constructor runs
// 2. The constructor checks for _BPFMAN_MNT_NS environment variable
// 3. If set, it calls setns() to enter that mount namespace
// 4. Go runtime then starts in the target mount namespace
// 5. If not set, Go starts normally (no namespace switch)
//
// # Logging
//
// The C code supports logging via _BPFMAN_NS_LOG_LEVEL environment variable:
//   - "debug" - verbose logging including namespace inodes
//   - "info"  - log namespace switches
//   - "error" - only log errors (default)
//   - "none"  - no logging
//
// # Usage
//
// To attach a uprobe in a container's mount namespace:
//
//	cmd := nsenter.CommandWithOptions(containerPid, selfPath, nsenter.CommandOptions{
//	    Mode: "bpfman-ns",
//	}, "uprobe", target, "--fn-name", fnName)
//	output, err := cmd.Output()
//
// The child process will:
// - Have its mount namespace switched before Go starts (via C constructor)
// - See the container's filesystem (target binary visible)
// - Access host bpffs via /proc/<host-pid>/root/sys/fs/bpf/...
package nsenter

/*
#cgo CFLAGS: -Wall
extern void nsexec(void);
void __attribute__((constructor)) init(void) {
	nsexec();
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

// Environment variable names used by nsenter.
const (
	// MntNsEnvVar triggers mount namespace switching in the C constructor.
	MntNsEnvVar = "_BPFMAN_MNT_NS"

	// LogLevelEnvVar controls C-level logging verbosity.
	// Values: "debug", "info", "error", "none"
	LogLevelEnvVar = "_BPFMAN_NS_LOG_LEVEL"

	// ModeEnvVar selects the binary's behavioural mode without
	// relying on argv[0] or symlinks. Valid values are
	// "bpfman-rpc" and "bpfman-ns".
	ModeEnvVar = "BPFMAN_MODE"
)

// LogLevel represents the logging verbosity for the C nsexec code.
type LogLevel string

const (
	LogLevelNone  LogLevel = "none"
	LogLevelError LogLevel = "error"
	LogLevelInfo  LogLevel = "info"
	LogLevelDebug LogLevel = "debug"
)

// CommandOptions configures how Command creates the child process.
type CommandOptions struct {
	// Logger for logging command creation (optional).
	Logger *slog.Logger

	// LogLevel sets the C-level logging for the child process.
	// Default is LogLevelError.
	LogLevel LogLevel

	// Mode sets BPFMAN_MODE for the child process ("bpfman-ns" or "bpfman-rpc").
	// Empty means do not set it.
	Mode string

	// NsPath overrides automatic namespace path detection.
	// If empty, uses /proc/<pid>/ns/mnt or /host/proc/<pid>/ns/mnt.
	NsPath string

	// ExtraFiles specifies additional open files to be inherited by the
	// child process. The files will be available as fd 3, 4, 5, etc.
	ExtraFiles []*os.File

	// WriterLockFD is a duped lock fd to pass to the child.
	// If non-nil, added to ExtraFiles and WriterLockEnvVar set.
	WriterLockFD *os.File

	// WriterLockEnvVar is the env var name for the lock fd.
	// Only used if WriterLockFD is non-nil.
	// Defaults to "BPFMAN_WRITER_LOCK_FD" if empty.
	WriterLockEnvVar string
}

// Command creates an exec.Cmd that will run in the mount namespace of the
// given container PID.
//
// The returned command, when executed, will:
// 1. Start with _BPFMAN_MNT_NS set to /proc/<containerPid>/ns/mnt
// 2. The C constructor (nsexec) runs before Go, calling setns()
// 3. Go runtime starts in the container's mount namespace
//
// The command inherits the current environment plus the namespace variable.
func Command(containerPid int32, name string, args ...string) *exec.Cmd {
	return CommandWithOptions(containerPid, name, CommandOptions{}, args...)
}

// CommandWithOptions creates an exec.Cmd with configurable options.
func CommandWithOptions(containerPid int32, name string, opts CommandOptions, args ...string) *exec.Cmd {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Determine namespace path
	nsPath := opts.NsPath
	if nsPath == "" {
		nsPath = fmt.Sprintf("/proc/%d/ns/mnt", containerPid)
		if _, err := os.Stat(nsPath); err != nil {
			altPath := fmt.Sprintf("/host/proc/%d/ns/mnt", containerPid)
			if _, err := os.Stat(altPath); err == nil {
				logger.Debug("using /host/proc namespace path",
					"original", nsPath,
					"actual", altPath)
				nsPath = altPath
			}
		}
	}

	// Get namespace inode for logging
	var nsInode uint64
	if stat, err := os.Stat(nsPath); err == nil {
		if sys, ok := stat.Sys().(*syscall.Stat_t); ok {
			nsInode = sys.Ino
		}
	}

	// Get current namespace inode for comparison
	var currentNsInode uint64
	if stat, err := os.Stat("/proc/self/ns/mnt"); err == nil {
		if sys, ok := stat.Sys().(*syscall.Stat_t); ok {
			currentNsInode = sys.Ino
		}
	}

	logger.Debug("creating namespace command",
		"container_pid", containerPid,
		"ns_path", nsPath,
		"target_ns_inode", nsInode,
		"current_ns_inode", currentNsInode,
		"executable", name,
		"args", args)

	cmd := exec.Command(name, args...)

	// Build environment with namespace variables
	logLevel := opts.LogLevel
	if logLevel == "" {
		logLevel = LogLevelError
	}

	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s", MntNsEnvVar, nsPath),
		fmt.Sprintf("%s=%s", LogLevelEnvVar, logLevel),
	)

	// Set BPFMAN_MODE if specified
	if opts.Mode != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", ModeEnvVar, opts.Mode))
	}

	// Pass any extra files (they become fd 3, 4, 5, ...)
	if len(opts.ExtraFiles) > 0 {
		cmd.ExtraFiles = opts.ExtraFiles
		logger.Debug("passing extra files to child",
			"count", len(opts.ExtraFiles))
	}

	// Pass writer lock fd if provided.
	// The child sees ExtraFiles[i] as fd 3+i.
	if opts.WriterLockFD != nil {
		idx := len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, opts.WriterLockFD)
		lockFdInChild := 3 + idx

		envVar := opts.WriterLockEnvVar
		if envVar == "" {
			envVar = "BPFMAN_WRITER_LOCK_FD" // fallback default
		}
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", envVar, lockFdInChild))

		logger.Debug("passing writer lock fd to child",
			"env_var", envVar,
			"fd_in_child", lockFdInChild)
	}

	logger.Debug("command environment configured",
		"MntNsEnvVar", nsPath,
		"LogLevelEnvVar", logLevel)

	return cmd
}

// CommandWithNsPath creates an exec.Cmd that will run in the mount namespace
// at the given path.
//
// This is a lower-level variant of Command that takes an explicit namespace
// path instead of a container PID.
func CommandWithNsPath(nsPath string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s", MntNsEnvVar, nsPath),
		fmt.Sprintf("%s=%s", LogLevelEnvVar, LogLevelError),
	)
	return cmd
}

// CommandWithNsPathAndLogger is like CommandWithNsPath but with logging.
func CommandWithNsPathAndLogger(nsPath string, logger *slog.Logger, logLevel LogLevel, name string, args ...string) *exec.Cmd {
	if logger == nil {
		logger = slog.Default()
	}

	// Get namespace inode for logging
	var nsInode uint64
	if stat, err := os.Stat(nsPath); err == nil {
		if sys, ok := stat.Sys().(*syscall.Stat_t); ok {
			nsInode = sys.Ino
		}
	}

	logger.Debug("creating namespace command with explicit path",
		"ns_path", nsPath,
		"ns_inode", nsInode,
		"executable", name,
		"args", args,
		"log_level", logLevel)

	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s", MntNsEnvVar, nsPath),
		fmt.Sprintf("%s=%s", LogLevelEnvVar, logLevel),
	)
	return cmd
}

// GetCurrentMntNsInode returns the inode of the current mount namespace.
// This is useful for logging and debugging namespace switches.
func GetCurrentMntNsInode() (uint64, error) {
	stat, err := os.Stat("/proc/self/ns/mnt")
	if err != nil {
		return 0, fmt.Errorf("stat /proc/self/ns/mnt: %w", err)
	}
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type")
	}
	return sys.Ino, nil
}
