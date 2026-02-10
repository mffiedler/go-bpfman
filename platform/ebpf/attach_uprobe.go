package ebpf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/ns/nsenter"
)

// AttachUprobeLocal attaches a pinned program to a user-space function
// in the current namespace. Does not spawn a helper, so no lock scope needed.
func (k *kernelAdapter) AttachUprobeLocal(ctx context.Context, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error) {
	k.logger.Debug("AttachUprobeLocal called",
		"target", target,
		"fn_name", fnName,
		"offset", offset,
		"retprobe", retprobe,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPinPath)

	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	// Regular uprobe - attach directly
	linkID, kernelLink, err := k.doAttachUprobeLocal(progPinPath, target, fnName, offset, retprobe, linkPinPath)
	if err != nil {
		return bpfman.AttachOutput{}, err
	}

	return bpfman.AttachOutput{
		LinkID:     linkID,
		KernelLink: kernelLink,
		PinPath:    linkPinPath,
	}, nil
}

// AttachUprobeContainer attaches a pinned program to a user-space function
// in a container's mount namespace. Spawns bpfman-ns helper, so requires
// lock scope to pass fd.
func (k *kernelAdapter) AttachUprobeContainer(ctx context.Context, scope lock.WriterScope, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string, containerPid int32) (bpfman.AttachOutput, error) {
	k.logger.Debug("AttachUprobeContainer called",
		"target", target,
		"fn_name", fnName,
		"offset", offset,
		"retprobe", retprobe,
		"container_pid", containerPid,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPinPath,
		"lock_fd", scope.FD())

	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	// Use bpfman-ns helper for container uprobes
	// scope is required - compiler enforces it (not nil)
	// Note: syntheticID is returned, not a kernel link ID (container uprobes use perf_event)
	syntheticID, err := k.attachUprobeViaHelper(scope, progPinPath, target, fnName, offset, retprobe, linkPinPath, containerPid)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach uprobe via helper: %w", err)
	}

	// Container uprobes use perf_event-based links which don't have kernel link IDs.
	// The syntheticID is stored as the durable ID in the database.
	// We also can't load the pinned link for container uprobes (they can't be pinned).
	return bpfman.AttachOutput{
		LinkID:     syntheticID,
		KernelLink: nil, // No kernel link for perf_event-based uprobes
		PinPath:    linkPinPath,
		Synthetic:  true,
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
		if err := pinWithRetry(lnk, linkPinPath); err != nil {
			lnk.Close()
			return 0, nil, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
		k.logger.Debug("link pinned successfully", "path", linkPinPath)
	}

	return linkID, ToKernelLink(linkInfo), nil
}

// attachUprobeViaHelper re-execs the current binary with CGO-based namespace
// switching to attach a uprobe in a container's mount namespace.
//
// Go's runtime is multi-threaded and setns(CLONE_NEWNS) requires a
// single-threaded process. We solve this using a CGO constructor (in the
// nsenter package) that runs before Go's runtime starts:
//
// 1. Parent creates socketpair for fd passing
// 2. Parent loads pinned program, passes fd via ExtraFiles (fd 3)
// 3. Parent passes socket via ExtraFiles (fd 4) for receiving link fd
// 4. Parent passes writer lock fd via ExtraFiles (fd 5) and env var
// 5. Parent sets _BPFMAN_MNT_NS env var and re-execs itself as "bpfman-ns"
// 6. Child's C constructor calls setns() before Go runtime starts
// 7. Child verifies it holds the writer lock
// 8. Child's Go code runs in target mount namespace (target binary visible)
// 9. Child uses inherited program fd to attach uprobe
// 10. Child sends link fd back to parent via socket (SCM_RIGHTS)
// 11. Parent receives link fd, keeps it open to maintain the uprobe
func (k *kernelAdapter) attachUprobeViaHelper(scope lock.WriterScope, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string, containerPid int32) (kernel.LinkID, error) {
	// Find the bpfman binary (which also serves as bpfman-ns)
	bpfmanPath, err := os.Executable()
	if err != nil {
		k.logger.Error("failed to get executable path", "error", err)
		return 0, fmt.Errorf("get executable path: %w", err)
	}

	// Load pinned program - we'll pass the fd to the child
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		k.logger.Error("failed to load pinned program", "path", progPinPath, "error", err)
		return 0, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
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
		return 0, fmt.Errorf("dup program fd: %w", err)
	}
	progFile := os.NewFile(uintptr(dupFd), "bpf-program")
	defer progFile.Close()

	// Create socketpair for receiving link fd from child.
	// Child will send the perf_event fd via SCM_RIGHTS.
	parentSocket, childSocket, err := nsenter.Socketpair()
	if err != nil {
		k.logger.Error("failed to create socketpair", "error", err)
		return 0, fmt.Errorf("create socketpair: %w", err)
	}
	defer parentSocket.Close()
	defer childSocket.Close()

	// Get current mount namespace inode for logging
	currentMntNs, _ := nsenter.GetCurrentMntNsInode()

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
			return 0, fmt.Errorf("container namespace for PID %d not accessible (tried %s and %s): %w", containerPid, nsPath, altPath, err)
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
	// Note: "bpfman-ns" mode is set via BPFMAN_MODE env var, not argv.
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
	childLogLevel := nsenter.LogLevelInfo
	if k.logger.Enabled(context.TODO(), slog.LevelDebug) {
		childLogLevel = nsenter.LogLevelDebug
	}

	// Dup the lock fd for the child process.
	// The child inherits the lock via the duped fd.
	lockFile, err := scope.DupFD()
	if err != nil {
		k.logger.Error("failed to dup lock fd for helper", "error", err)
		return 0, fmt.Errorf("dup lock fd for helper: %w", err)
	}
	defer lockFile.Close() // Close parent's dup after child starts

	// Use nsenter.CommandWithOptions with ExtraFiles to pass program fd, socket, and lock fd
	cmd := nsenter.CommandWithOptions(containerPid, bpfmanPath, nsenter.CommandOptions{
		Logger:           k.logger,
		LogLevel:         childLogLevel,
		Mode:             "bpfman-ns",
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

	// Start the child process
	cmd.Stderr = os.Stderr // Inherit stderr for helper logging

	if err := cmd.Start(); err != nil {
		k.logger.Error("failed to start bpfman-ns helper",
			"error", err,
			"container_pid", containerPid,
			"ns_path", nsPath)
		return 0, fmt.Errorf("start bpfman-ns for container %d: %w", containerPid, err)
	}

	// Close child's socket end in parent - child has its own copy via ExtraFiles
	childSocket.Close()

	// Receive link fd from child via socket
	k.logger.Debug("waiting for link fd from child")
	linkFd, name, err := nsenter.RecvFd(parentSocket)
	if err != nil {
		k.logger.Error("failed to receive link fd from child",
			"error", err,
			"container_pid", containerPid)
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("receive link fd from child: %w", err)
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
				"ns_path", nsPath)
			syscall.Close(linkFd) // Clean up received fd
			return 0, fmt.Errorf("bpfman-ns failed attaching %s to %q in container %d (exit %d)", fnName, target, containerPid, exitErr.ExitCode())
		}
		k.logger.Error("failed to wait for bpfman-ns helper",
			"error", err,
			"container_pid", containerPid)
		syscall.Close(linkFd)
		return 0, fmt.Errorf("wait for bpfman-ns: %w", err)
	}

	// We now have the link fd. For perf_event-based uprobes, we cannot pin them.
	// We keep the fd open to maintain the uprobe attachment.
	// The link will be released when this fd is closed.
	k.logger.Info("container uprobe attachment succeeded",
		"link_fd", linkFd,
		"container_pid", containerPid,
		"target", target,
		"fn_name", fnName)

	// Perf_event-based links cannot be pinned to bpffs. We store the fd in a
	// map to keep the uprobe attached for the lifetime of this process.
	// The key uniquely identifies this attachment.
	linkKey := fmt.Sprintf("%d:%s:%s", containerPid, target, fnName)
	k.linkFds.Store(linkKey, linkFd)

	// Generate a synthetic link ID for database storage. Real kernel link IDs
	// are small sequential numbers; synthetic IDs are in range 0x80000000+ to
	// avoid collision. This allows the database to maintain a unique constraint
	// on link IDs while supporting perf_event-based attachments that lack
	// kernel link IDs.
	syntheticID := generateSyntheticLinkID()

	k.logger.Info("stored link fd for container uprobe",
		"key", linkKey,
		"link_fd", linkFd,
		"synthetic_link_id", syntheticID,
		"note", "perf_event links cannot be pinned; link will be released when daemon exits")

	return syntheticID, nil
}
