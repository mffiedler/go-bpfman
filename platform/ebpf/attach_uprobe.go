package ebpf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfman/ns"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// AttachUprobeLocal attaches a pinned program to a user-space function
// in the current namespace. Does not spawn a helper, so no lock scope needed.
func (k *kernelAdapter) AttachUprobeLocal(ctx context.Context, progPinPath bpfman.ProgPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	linkPin := linkPinPath.String()
	k.logger.Debug("AttachUprobeLocal called",
		"target", target,
		"fn_name", fnName,
		"offset", offset,
		"retprobe", retprobe,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPin)

	prog, err := ebpf.LoadPinnedProgram(progPinPath.String(), nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	// Regular uprobe - attach directly
	linkID, kernelLink, err := k.doAttachUprobeLocal(progPinPath.String(), target, fnName, offset, retprobe, linkPin)
	if err != nil {
		return bpfman.AttachOutput{}, err
	}

	return bpfman.AttachOutput{
		KernelLinkID: &linkID,
		KernelLink:   kernelLink,
		PinPath:      linkPinPath,
	}, nil
}

// AttachUprobeContainer attaches a pinned program to a user-space function
// in a container's mount namespace. Spawns bpfman-ns helper, so requires
// lock scope to pass fd.
func (k *kernelAdapter) AttachUprobeContainer(ctx context.Context, scope lock.WriterScope, progPinPath bpfman.ProgPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath bpfman.LinkPath, containerPid int32) (bpfman.AttachOutput, error) {
	linkPin := linkPinPath.String()
	k.logger.Debug("AttachUprobeContainer called",
		"target", target,
		"fn_name", fnName,
		"offset", offset,
		"retprobe", retprobe,
		"container_pid", containerPid,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPin,
		"lock_fd", scope.FD())

	prog, err := ebpf.LoadPinnedProgram(progPinPath.String(), nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	linkID, kernelLink, err := k.attachUprobeViaHelper(scope, progPinPath.String(), target, fnName, offset, retprobe, linkPin, containerPid)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach uprobe via helper: %w", err)
	}

	return bpfman.AttachOutput{
		KernelLinkID: &linkID,
		KernelLink:   kernelLink,
		PinPath:      linkPinPath,
	}, nil
}

// doAttachUprobeLocal attaches a uprobe directly (no namespace switching).
func (k *kernelAdapter) doAttachUprobeLocal(progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string) (kernel.LinkID, *kernel.Link, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	ex, err := link.OpenExecutable(target)
	if err != nil {
		return 0, nil, fmt.Errorf("open executable %s: %w", target, err)
	}

	opts := &link.UprobeOptions{Offset: offset}
	var lnk link.Link
	if retprobe {
		lnk, err = ex.Uretprobe(fnName, prog, opts)
	} else {
		lnk, err = ex.Uprobe(fnName, prog, opts)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("attach uprobe to %s in %s: %w", fnName, target, err)
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return 0, nil, fmt.Errorf("get link info: %w", err)
	}
	linkID := kernel.LinkID(linkInfo.ID)

	k.logger.Debug("uprobe link created", "link_id", linkID, "link_type", linkInfo.Type)

	// Pin the link if path provided
	if linkPinPath != "" {
		if err := pinWithRetry(linkPinPath, lnk.Pin); err != nil {
			lnk.Close()
			return 0, nil, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
		k.logger.Debug("link pinned successfully", "path", linkPinPath)
	}

	// Hand the live link to the kernelAdapter so DetachLink can
	// Close it after unpinning. Pin-removal alone does not run
	// perf_event_free_bpf_prog for probe-style attachments.
	if linkPinPath != "" {
		k.trackLink(linkPinPath, lnk)
	} else {
		lnk.Close()
	}

	return linkID, ToKernelLink(linkInfo), nil
}

// attachUprobeViaHelper re-execs the current binary with CGO-based namespace
// switching to attach a uprobe in a container's mount namespace.
//
// Go's runtime is multi-threaded and setns(CLONE_NEWNS) requires a
// single-threaded process. We solve this using a CGO constructor in the
// bpfman-ns transport that runs before Go's runtime starts:
//
// 1. Parent creates socketpair for fd passing
// 2. Parent loads pinned program, passes fd via ExtraFiles (fd 3)
// 3. Parent passes socket via ExtraFiles (fd 4) for receiving link fd
// 4. Parent passes writer lock fd via ExtraFiles (fd 5) and env var
// 5. Parent sets _BPFMAN_MNT_NS env var and re-execs itself in bpfman-ns mode
// 6. Child's C constructor calls setns() before Go runtime starts
// 7. Child verifies it holds the writer lock
// 8. Child's Go code runs in target mount namespace (target binary visible)
// 9. Child uses inherited program fd to attach uprobe
// 10. Child sends link fd back to parent via socket (SCM_RIGHTS)
// 11. Parent pins the received link fd in host bpffs
func (k *kernelAdapter) attachUprobeViaHelper(scope lock.WriterScope, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string, containerPid int32) (kernel.LinkID, *kernel.Link, error) {
	// Find the bpfman binary (which also serves as bpfman-ns)
	bpfmanPath, err := os.Executable()
	if err != nil {
		k.logger.Error("failed to get executable path", "error", err)
		return 0, nil, fmt.Errorf("get executable path: %w", err)
	}

	// Load pinned program - we'll pass the fd to the child
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		k.logger.Error("failed to load pinned program", "path", progPinPath, "error", err)
		return 0, nil, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	progInfo, _ := prog.Info()
	progID, _ := progInfo.ID()

	// Dup the program fd so we can pass it to child via ExtraFiles.
	// This avoids ownership issues between ebpf.Program and os.File.
	progFd := prog.FD()
	dupFd, err := syscall.Dup(progFd)
	if err != nil {
		k.logger.Error("failed to dup program fd", "fd", progFd, "error", err)
		return 0, nil, fmt.Errorf("dup program fd: %w", err)
	}
	progFile := os.NewFile(uintptr(dupFd), "bpf-program")
	defer progFile.Close()

	// Create socketpair for receiving link fd from child.
	// Child will send the perf_event fd via SCM_RIGHTS.
	parentSocket, childSocket, err := ns.Socketpair()
	if err != nil {
		k.logger.Error("failed to create socketpair", "error", err)
		return 0, nil, fmt.Errorf("create socketpair: %w", err)
	}
	defer parentSocket.Close()
	defer childSocket.Close()

	// Get current mount namespace inode for logging
	currentMntNs, _ := ns.GetCurrentMntNsInode()

	// Determine target namespace path - try /proc first, then /host/proc for k8s
	nsPath := fmt.Sprintf("/proc/%d/ns/mnt", containerPid)
	if _, err := os.Stat(nsPath); err != nil {
		altPath := fmt.Sprintf("/host/proc/%d/ns/mnt", containerPid)
		if _, err := os.Stat(altPath); err != nil {
			k.logger.Error("container namespace not accessible",
				"container_pid", containerPid,
				"tried_paths", []string{nsPath, altPath},
				"error", err,
				"hint", "ensure container PID is valid and /proc or /host/proc is accessible")
			return 0, nil, fmt.Errorf("container namespace for PID %d not accessible (tried %s and %s): %w", containerPid, nsPath, altPath, err)
		}
		nsPath = altPath
	}

	k.logger.Info("preparing container uprobe attachment",
		"container_pid", containerPid,
		"current_mnt_ns_inode", currentMntNs,
		"target_ns_path", nsPath,
		"target_binary", target,
		"fn_name", fnName,
		"offset", offset,
		"retprobe", retprobe,
		"prog_pin_path", progPinPath,
		"prog_id", progID,
		"link_pin_path", linkPinPath)

	// Build arguments for bpfman-ns uprobe command.
	// Program fd passed via ExtraFiles[0] (fd 3 in child).
	// Socket fd passed via ExtraFiles[1] (fd 4 in child) for returning link fd.
	// Note: bpfman-ns mode is set via BPFMAN_MODE env var, not argv.
	args := []string{
		"uprobe",
		target,
		"--fn-name", fnName,
		"--offset", fmt.Sprintf("%d", offset),
	}
	if retprobe {
		args = append(args, "--retprobe")
	}

	// Determine log level for child process based on our logger's level
	childLogLevel := ns.LogLevelInfo
	if k.logger.Enabled(context.TODO(), slog.LevelDebug) {
		childLogLevel = ns.LogLevelDebug
	}

	// Dup the lock fd for the child process.
	// The child inherits the lock via the duped fd.
	lockFile, err := scope.DupFD()
	if err != nil {
		k.logger.Error("failed to dup lock fd for helper", "error", err)
		return 0, nil, fmt.Errorf("dup lock fd for helper: %w", err)
	}
	defer lockFile.Close() // Close parent's dup after child starts

	// Use the shared bpfman-ns transport to pass program, socket, and lock fds.
	cmd := ns.CommandWithOptions(containerPid, bpfmanPath, ns.CommandOptions{
		Logger:           k.logger,
		LogLevel:         childLogLevel,
		Mode:             ns.ModeBPFManNS,
		NsPath:           nsPath,
		ExtraFiles:       []*os.File{progFile, childSocket}, // fd 3, fd 4 in child
		WriterLockFD:     lockFile,
		WriterLockEnvVar: lock.WriterLockFDEnvVar,
	}, args...)

	k.logger.Debug("executing bpfman-ns helper subprocess",
		"executable", bpfmanPath,
		"args", args,
		"child_log_level", childLogLevel,
		"program_fd_passed", true,
		"socket_fd_passed", true)

	var helperStderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &helperStderr)

	if err := cmd.Start(); err != nil {
		k.logger.Error("failed to start bpfman-ns helper",
			"error", err,
			"container_pid", containerPid,
			"ns_path", nsPath)
		return 0, nil, fmt.Errorf("start bpfman-ns for container %d: %w", containerPid, err)
	}

	// Close child's socket end in parent - child has its own copy via ExtraFiles
	childSocket.Close()

	// Receive link fd from child via socket
	k.logger.Debug("waiting for link fd from child")
	linkFd, name, err := ns.RecvFd(parentSocket)
	if err != nil {
		k.logger.Error("failed to receive link fd from child",
			"error", err,
			"container_pid", containerPid)
		cmd.Process.Kill()
		waitErr := cmd.Wait()
		return 0, nil, helperReceiveError(fnName, target, containerPid, err, waitErr, helperStderr.String())
	}
	k.logger.Debug("received link fd from child",
		"link_fd", linkFd,
		"name", name)

	// Wait for child to exit - exit 0 signals success
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			k.logger.Error("bpfman-ns helper failed",
				"exit_code", exitErr.ExitCode(),
				"container_pid", containerPid,
				"target", target,
				"fn_name", fnName,
				"helper_stderr", summariseHelperStderr(helperStderr.String()),
				"ns_path", nsPath)
			syscall.Close(linkFd) // Clean up received fd
			return 0, nil, helperExitError(fnName, target, containerPid, exitErr.ExitCode(), helperStderr.String())
		}
		k.logger.Error("failed to wait for bpfman-ns helper",
			"error", err,
			"container_pid", containerPid)
		syscall.Close(linkFd)
		return 0, nil, fmt.Errorf("wait for bpfman-ns: %w", err)
	}

	lnk, err := link.NewFromFD(linkFd)
	if err != nil {
		return 0, nil, fmt.Errorf("wrap container uprobe BPF link fd: %w", err)
	}
	if err := pinWithRetry(linkPinPath, lnk.Pin); err != nil {
		lnk.Close()
		return 0, nil, fmt.Errorf("pin container uprobe link to %s: %w", linkPinPath, err)
	}
	info, err := lnk.Info()
	if err != nil {
		if unpinErr := lnk.Unpin(); unpinErr != nil {
			k.logger.Warn("failed to unpin container uprobe link after info error",
				"pin_path", linkPinPath,
				"error", unpinErr)
		}
		lnk.Close()
		return 0, nil, fmt.Errorf("get container uprobe link info: %w", err)
	}

	linkID := kernel.LinkID(info.ID)
	k.trackLink(linkPinPath, lnk)

	k.logger.Info("container uprobe link pinned",
		"pin_path", linkPinPath,
		"kernel_link_id", uint32(linkID),
		"link_type", info.Type,
		"program_id", uint32(info.Program))

	return linkID, ToKernelLink(info), nil
}

func helperExitError(fnName, target string, containerPid int32, exitCode int, stderr string) error {
	reason := summariseHelperStderr(stderr)
	if reason == "" {
		return fmt.Errorf("bpfman-ns failed attaching %s to %q in container %d (exit %d)", fnName, target, containerPid, exitCode)
	}
	return fmt.Errorf("bpfman-ns failed attaching %s to %q in container %d (exit %d): %s", fnName, target, containerPid, exitCode, reason)
}

func helperReceiveError(fnName, target string, containerPid int32, recvErr, waitErr error, stderr string) error {
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && summariseHelperStderr(stderr) != "" {
		return helperExitError(fnName, target, containerPid, exitErr.ExitCode(), stderr)
	}
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return fmt.Errorf("receive link fd from child: %w; wait for bpfman-ns: %v", recvErr, waitErr)
	}
	return fmt.Errorf("receive link fd from child: %w", recvErr)
}

// nsexecErrorPattern matches the C constructor's error lines
// ("nsexec[<pid>]: ERROR: ..."), emitted when setns fails before the Go
// runtime starts.
var nsexecErrorPattern = regexp.MustCompile(`^nsexec\[[0-9]+\]: ERROR: `)

// summariseHelperStderr returns the failure reason from the helper's
// stderr, or "" when it holds none. Only the error shapes this codebase
// emits count as a reason: the child CLI's final "bpfman-ns: error:" or
// "bpfman-shell: error:" line, and the C constructor's nsexec ERROR
// line. The helper logs chatter at info level by default, so stderr is
// rarely empty; matching on shape rather than taking the last non-empty
// line keeps a routine log line from being reported as the failure
// reason when the helper dies without printing one.
func summariseHelperStderr(stderr string) string {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if reason := helperErrorReason(strings.TrimSpace(lines[i])); reason != "" {
			return reason
		}
	}
	return ""
}

func helperErrorReason(line string) string {
	for _, prefix := range []string{
		"bpfman-ns: error:",
		"bpfman-shell: error:",
	} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	if nsexecErrorPattern.MatchString(line) {
		return line
	}
	return ""
}
