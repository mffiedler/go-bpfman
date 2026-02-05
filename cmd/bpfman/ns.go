// Package cli provides the command-line interface for bpfman.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/nsenter"
)

// NSCmd handles the bpfman-ns subcommand for attaching uprobes in other
// namespaces.
//
// The namespace switch happens via a CGO constructor (in the nsenter package)
// that runs before Go's runtime starts. The parent process sets _BPFMAN_MNT_NS
// environment variable, and the C code calls setns(CLONE_NEWNS) while still
// single-threaded.
type NSCmd struct {
	Uprobe NSUprobeCmd `cmd:"" help:"Attach uprobe in target namespace."`
}

// File descriptor numbers for inherited fds from parent.
// The parent uses Cmd.ExtraFiles which maps to fd 3, 4, 5, etc.
// WriterLockFD is passed via environment variable since its position
// in ExtraFiles may vary.
const (
	ProgramFD = 3 // BPF program fd
	SocketFD  = 4 // Unix socket for passing link fd back to parent
)

// NSUprobeCmd attaches a uprobe in the target container's mount namespace.
// When this code runs, the process is already in the target namespace
// (switched by the CGO constructor before Go started).
//
// The parent process passes:
//   - BPF program via fd 3 (ExtraFiles[0])
//   - Unix socket via fd 4 (ExtraFiles[1]) for returning the link fd
//
// After attaching, we send the link fd back to the parent via the socket.
// The parent (in host namespace) then pins the link.
type NSUprobeCmd struct {
	Target   string `arg:"" help:"Target binary path (resolved in container namespace)."`
	FnName   string `name:"fn-name" help:"Function name to attach to."`
	Offset   uint64 `name:"offset" default:"0" help:"Offset from function start."`
	Retprobe bool   `name:"retprobe" help:"Attach as uretprobe."`
}

// getMntNsInode returns the inode of a mount namespace file.
func getMntNsInode(path string) uint64 {
	stat, err := os.Stat(path)
	if err != nil {
		return 0
	}
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return sys.Ino
}

// Run executes the uprobe attachment. We're already in the target namespace
// (the CGO constructor called setns before Go started).
//
// The BPF program is passed via fd 3, and a Unix socket via fd 4.
// The writer lock fd is passed via the BPFMAN_WRITER_LOCK_FD environment variable.
// After attaching, we send the link fd back to the parent over the socket.
func (cmd *NSUprobeCmd) Run() error {
	// Create a logger that writes to stderr, respecting BPFMAN_LOG if set.
	// Defaults to info level for less verbose output in normal operation.
	logLevel := slog.LevelInfo
	if spec := os.Getenv("BPFMAN_LOG"); spec != "" {
		// Simple level extraction: take first word if it looks like a level
		// (full spec parsing is overkill for the helper subprocess)
		switch spec {
		case "trace":
			logLevel = slog.Level(-8) // LevelTrace
		case "debug":
			logLevel = slog.LevelDebug
		case "warn", "warning":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Verify writer lock - helper cannot proceed without it.
	// The lock fd is passed via environment variable since its position
	// in ExtraFiles may vary.
	fdStr := os.Getenv(lock.WriterLockFDEnvVar)
	if fdStr == "" {
		logger.Error("writer lock fd not set",
			"env_var", lock.WriterLockFDEnvVar,
			"hint", "bpfman-ns must be spawned with lock fd")
		return fmt.Errorf("%s not set: bpfman-ns must be spawned with lock fd", lock.WriterLockFDEnvVar)
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		logger.Error("invalid writer lock fd",
			"env_var", lock.WriterLockFDEnvVar,
			"value", fdStr,
			"error", err)
		return fmt.Errorf("invalid %s=%q: %w", lock.WriterLockFDEnvVar, fdStr, err)
	}

	scope, err := lock.InheritedLockFromFD(fd)
	if err != nil {
		logger.Error("lock verification failed",
			"fd", fd,
			"error", err,
			"hint", "parent must hold exclusive lock before spawning helper")
		return fmt.Errorf("lock verification failed: %w", err)
	}
	defer scope.Close()

	logger.Debug("writer lock verified",
		"fd", fd)

	// Log our current state
	currentMntNs := getMntNsInode("/proc/self/ns/mnt")

	logger.Info("bpfman-ns uprobe handler started",
		"pid", os.Getpid(),
		"ppid", os.Getppid(),
		"current_mnt_ns_inode", currentMntNs,
		"target", cmd.Target,
		"fn_name", cmd.FnName,
		"offset", cmd.Offset,
		"retprobe", cmd.Retprobe,
		"program_fd", ProgramFD,
		"socket_fd", SocketFD)

	// Create program from the inherited file descriptor.
	// The parent opened the pinned program and passed the fd via ExtraFiles.
	// The child process owns its copy of the fd after exec.
	logger.Debug("creating program from inherited fd", "fd", ProgramFD)
	prog, err := ebpf.NewProgramFromFD(ProgramFD)
	if err != nil {
		logger.Error("failed to create program from fd",
			"fd", ProgramFD,
			"error", err,
			"hint", "bpfman-ns must be invoked by the daemon, not directly")
		return fmt.Errorf("create program from fd %d (bpfman-ns must be invoked by daemon, not directly): %w", ProgramFD, err)
	}
	defer prog.Close()
	logger.Debug("program from inherited fd",
		"fd", ProgramFD,
		"prog_type", prog.Type())

	// Get the socket for sending link fd back to parent
	socket := os.NewFile(uintptr(SocketFD), "fdpass-socket")
	if socket == nil {
		logger.Error("failed to get socket fd",
			"fd", SocketFD,
			"hint", "bpfman-ns must be invoked by the daemon, not directly")
		return fmt.Errorf("socket fd %d not available (bpfman-ns must be invoked by daemon)", SocketFD)
	}
	defer socket.Close()

	// Verify target binary exists in current namespace.
	if stat, err := os.Stat(cmd.Target); err != nil {
		logger.Error("target binary not found in container namespace",
			"target", cmd.Target,
			"error", err,
			"current_mnt_ns_inode", currentMntNs,
			"hint", "ensure the target path exists in the container's filesystem")
		return fmt.Errorf("target binary %q not found in container (mnt ns inode %d): %w", cmd.Target, currentMntNs, err)
	} else {
		logger.Debug("target binary found in container namespace",
			"target", cmd.Target,
			"size", stat.Size(),
			"mode", stat.Mode())
	}

	// Open the executable (resolves in current/target namespace)
	logger.Debug("opening executable", "target", cmd.Target)
	ex, err := link.OpenExecutable(cmd.Target)
	if err != nil {
		logger.Error("failed to open executable",
			"target", cmd.Target,
			"error", err)
		return fmt.Errorf("open executable %s: %w", cmd.Target, err)
	}
	logger.Debug("opened executable", "target", cmd.Target)

	// Attach uprobe
	opts := &link.UprobeOptions{Offset: cmd.Offset}
	var lnk link.Link

	attachType := "uprobe"
	if cmd.Retprobe {
		attachType = "uretprobe"
	}
	logger.Info("attaching probe",
		"type", attachType,
		"fn_name", cmd.FnName,
		"offset", cmd.Offset,
		"target", cmd.Target)

	if cmd.Retprobe {
		lnk, err = ex.Uretprobe(cmd.FnName, prog, opts)
	} else {
		lnk, err = ex.Uprobe(cmd.FnName, prog, opts)
	}
	if err != nil {
		logger.Error("failed to attach probe",
			"type", attachType,
			"fn_name", cmd.FnName,
			"offset", cmd.Offset,
			"target", cmd.Target,
			"current_mnt_ns_inode", currentMntNs,
			"error", err)
		return fmt.Errorf("attach %s to %s (offset %d) in %q (mnt ns %d): %w", attachType, cmd.FnName, cmd.Offset, cmd.Target, currentMntNs, err)
	}

	logger.Info("probe attached successfully", "type", attachType)

	// Get the perf event fd from the link.
	// Uprobe links implement the PerfEvent interface.
	pe, ok := lnk.(link.PerfEvent)
	if !ok {
		logger.Error("link does not implement PerfEvent interface",
			"type", attachType)
		lnk.Close()
		return fmt.Errorf("link does not implement PerfEvent interface")
	}

	perfFile, err := pe.PerfEvent()
	if err != nil {
		logger.Error("failed to get perf event fd",
			"error", err)
		lnk.Close()
		return fmt.Errorf("get perf event fd: %w", err)
	}

	// Send the perf event fd back to the parent via the Unix socket.
	// The parent (in host namespace) will receive it and keep the link alive.
	linkFd := int(perfFile.Fd())
	logger.Debug("sending link fd to parent",
		"link_fd", linkFd,
		"socket_fd", SocketFD)

	if err := nsenter.SendFd(socket, "uprobe-link", linkFd); err != nil {
		logger.Error("failed to send link fd to parent",
			"link_fd", linkFd,
			"error", err)
		perfFile.Close()
		lnk.Close()
		return fmt.Errorf("send link fd to parent: %w", err)
	}

	logger.Info("link fd sent to parent successfully",
		"link_fd", linkFd)

	// Close our references. The parent now has the fd via SCM_RIGHTS.
	// The perf event fd we sent keeps the attachment alive in the parent.
	// Success is signalled by clean exit (exit 0); parent uses Wait() result.
	perfFile.Close()
	lnk.Close()

	return nil
}

// NamespaceHelperInvocation captures the details of a namespace helper
// invocation. This is used when bpfman re-execs itself to attach uprobes
// inside container mount namespaces.
type NamespaceHelperInvocation struct {
	Args []string
}

// DetectNamespaceHelperInvocation checks if this invocation is for the
// namespace helper subprocess (used for container uprobe attachment).
//
// Detection logic:
//  1. If BPFMAN_MODE is set:
//     - "bpfman-ns" → helper mode
//     - "bpfman-rpc" → not helper (valid, but different mode)
//     - anything else → error (unknown mode)
//  2. If BPFMAN_MODE is not set:
//     - argv[0] basename is "bpfman-ns" → helper mode (symlink compatibility)
//     - otherwise → not helper
//
// Returns the invocation details (with rewritten args for the helper parser),
// whether helper mode was detected, and any error for invalid configuration.
func DetectNamespaceHelperInvocation(argv []string, modeEnv string) (NamespaceHelperInvocation, bool, error) {
	if len(argv) == 0 {
		return NamespaceHelperInvocation{}, false, nil
	}

	// If BPFMAN_MODE is set, it takes precedence and must be valid
	if modeEnv != "" {
		switch modeEnv {
		case "bpfman-ns":
			return NamespaceHelperInvocation{Args: argv[1:]}, true, nil
		case "bpfman-rpc":
			return NamespaceHelperInvocation{}, false, nil
		default:
			return NamespaceHelperInvocation{}, false, fmt.Errorf("unknown BPFMAN_MODE=%q; valid values: bpfman-ns, bpfman-rpc", modeEnv)
		}
	}

	// Fall back to argv[0] check for symlink compatibility
	if filepath.Base(argv[0]) == "bpfman-ns" {
		return NamespaceHelperInvocation{Args: argv[1:]}, true, nil
	}

	return NamespaceHelperInvocation{}, false, nil
}

// HandleNamespaceHelperInvocation detects namespace helper mode and runs the
// provided runner if detected. Returns whether the invocation was handled and
// any error from detection or the runner.
func HandleNamespaceHelperInvocation(argv []string, modeEnv string, run func(NamespaceHelperInvocation) error) (handled bool, err error) {
	inv, isHelper, err := DetectNamespaceHelperInvocation(argv, modeEnv)
	if err != nil {
		return false, err
	}
	if !isHelper {
		return false, nil
	}
	return true, run(inv)
}

// runNamespaceHelper is the default runner for namespace helper invocations.
// It parses the helper CLI and executes the command without mutating os.Args.
func runNamespaceHelper(inv NamespaceHelperInvocation) error {
	var cmd NSCmd

	parser, err := kong.New(&cmd,
		kong.Name("bpfman-ns"),
		kong.Description("BPF namespace subprocess for container uprobes."),
		kong.UsageOnError(),
	)
	if err != nil {
		return fmt.Errorf("create parser: %w", err)
	}

	ctx, err := parser.Parse(inv.Args)
	if err != nil {
		return err
	}

	return ctx.Run()
}
