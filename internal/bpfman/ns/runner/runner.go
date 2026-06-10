package runner

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman/internal/bpfman/ns"
	"github.com/frobware/go-bpfman/lock"
)

// NSCmd handles the bpfman-ns subcommand for attaching uprobes in other
// namespaces.
//
// The namespace switch happens via a CGO constructor (in the ns package)
// that runs before Go's runtime starts. The parent process sets _BPFMAN_MNT_NS
// environment variable, and the C code calls setns(CLONE_NEWNS) while still
// single-threaded.
type NSCmd struct {
	Uprobe NSUprobeCmd `cmd:"" help:"Attach a uprobe program in the given container."`
}

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
		"program_fd", ns.ProgramFD,
		"socket_fd", ns.SocketFD)

	// Create program from the inherited file descriptor.
	// The parent opened the pinned program and passed the fd via ExtraFiles.
	// The child process owns its copy of the fd after exec.
	logger.Debug("creating program from inherited fd", "fd", ns.ProgramFD)
	prog, err := ebpf.NewProgramFromFD(ns.ProgramFD)
	if err != nil {
		logger.Error("failed to create program from fd",
			"fd", ns.ProgramFD,
			"error", err,
			"hint", "bpfman-ns must be invoked by the daemon, not directly")
		return fmt.Errorf("create program from fd %d (bpfman-ns must be invoked by daemon, not directly): %w", ns.ProgramFD, err)
	}
	defer prog.Close()
	logger.Debug("program from inherited fd",
		"fd", ns.ProgramFD,
		"prog_type", prog.Type())

	// Get the socket for sending link fd back to parent
	socket := os.NewFile(uintptr(ns.SocketFD), "fdpass-socket")
	if socket == nil {
		logger.Error("failed to get socket fd",
			"fd", ns.SocketFD,
			"hint", "bpfman-ns must be invoked by the daemon, not directly")
		return fmt.Errorf("socket fd %d not available (bpfman-ns must be invoked by daemon)", ns.SocketFD)
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

	// Send the fd that owns the attachment back to the parent via the Unix
	// socket. Container uprobes must return a BPF link fd so the host-namespace
	// parent can pin it and record its kernel link ID. Older ioctl-style
	// perf-event attachments only expose a raw perf-event fd; that fd cannot be
	// pinned as a BPF link, so accepting it would recreate the one-shot phantom
	// link bug.
	raw, ok := lnk.(interface{ FD() int })
	if !ok {
		logger.Error("container uprobe requires a pinnable BPF link fd",
			"type", attachType,
			"hint", "upgrade to a kernel/Cilium path that exposes BPF perf-event links")
		lnk.Close()
		return fmt.Errorf("container uprobe requires a pinnable BPF link fd; raw perf-event attachments are not supported")
	}
	linkFd := raw.FD()

	// The parent (in host namespace) receives its own fd reference and keeps
	// it open for the lifetime of the managed link.
	logger.Debug("sending link fd to parent",
		"link_fd", linkFd,
		"socket_fd", ns.SocketFD)

	if err := ns.SendFd(socket, ns.LinkFDName, linkFd); err != nil {
		logger.Error("failed to send link fd to parent",
			"link_fd", linkFd,
			"error", err)
		lnk.Close()
		return fmt.Errorf("send link fd to parent: %w", err)
	}

	logger.Info("link fd sent to parent successfully",
		"link_fd", linkFd)

	// Close our references. The parent now has the fd via SCM_RIGHTS.
	// Success is signalled by clean exit (exit 0); parent uses Wait() result.
	lnk.Close()

	return nil
}

// NamespaceHelperInvocation captures the details of a namespace helper
// invocation. This is used when a binary re-execs itself to attach uprobes
// inside container mount namespaces.
type NamespaceHelperInvocation struct {
	Args []string
}

// DetectNamespaceHelperInvocation checks if this invocation is for the
// namespace helper subprocess (used for container uprobe attachment).
//
// Detection logic:
//   - "bpfman-ns" → helper mode
//   - "bpfman-rpc" → not helper (valid, but different mode)
//   - anything else → error (unknown mode)
//
// Returns the invocation details (with rewritten args for the helper parser),
// whether helper mode was detected, and any error for invalid configuration.
func DetectNamespaceHelperInvocation(argv []string, modeEnv string) (NamespaceHelperInvocation, bool, error) {
	if len(argv) == 0 || modeEnv == "" {
		return NamespaceHelperInvocation{}, false, nil
	}

	switch modeEnv {
	case ns.ModeBPFManNS:
		return NamespaceHelperInvocation{Args: argv[1:]}, true, nil
	case ns.ModeBPFManRPC:
		return NamespaceHelperInvocation{}, false, nil
	default:
		return NamespaceHelperInvocation{}, false, fmt.Errorf("unknown BPFMAN_MODE=%q; valid values: bpfman-ns, bpfman-rpc", modeEnv)
	}
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
// It parses the helper arguments and executes the command without mutating os.Args.
func runNamespaceHelper(inv NamespaceHelperInvocation) error {
	var cmd NSCmd

	parser, err := kong.New(&cmd,
		kong.Name(ns.ModeBPFManNS),
		kong.Description("Attach an eBPF program inside a container's mount namespace."),
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

// Run checks whether this process was re-execed as the bpfman-ns helper
// (container uprobe attachment) and, if so, parses and runs the helper
// command. The returned ran is true when the helper path was taken; the
// caller should then exit without proceeding to its normal CLI. err is set
// for an invalid invocation (ran false) or a helper failure (ran true).
func Run() (ran bool, err error) {
	return HandleNamespaceHelperInvocation(os.Args, os.Getenv(ns.ModeEnvVar), runNamespaceHelper)
}
