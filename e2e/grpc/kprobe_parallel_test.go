//go:build e2e

// Package grpcparallel exercises the bpfman gRPC daemon under
// concurrent client load. The test launches a real `bpfman serve`
// subprocess against a tmp suite root, opens a shared gRPC client
// connection, and fans N goroutines through the full
// load/get/attach/getlink/detach/unload cycle against the daemon.
//
// It is the parallel companion to the
// e2e/new/Test*_LoadAttachDetachUnload.bpfman scripts. The scripts
// remain the canonical correctness suite (exhaustive shape matches);
// this test is deliberately thin and exists to stress the surface
// the in-process-mutex removal opened up: read RPCs running lockless
// alongside flock-serialised writers inside one daemon process.
//
// Run from the repository root:
//
//	sudo make test-e2e EXTRA_GOFLAGS='-run=TestKprobeParallel_GRPC' TEST_PATH=./e2e/grpc/...
//
// Knobs:
//
//	BPFMAN_GRPC_PARALLEL_N      goroutines (default 32)
//	BPFMAN_GRPC_PARALLEL_ITERS  iterations per goroutine (default 4)
//	BPFMAN_BIN                  path to bpfman binary (default <repo>/bin/bpfman)
package grpcparallel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/frobware/go-bpfman/server/pb"
)

var (
	// client is the shared gRPC client. pb.BpfmanClient is
	// goroutine-safe; one connection serves N concurrent test
	// goroutines, which matches the production topology of a
	// single CLI fanning RPCs into the daemon.
	client pb.BpfmanClient

	// kprobeObjectPath is the absolute path the daemon will open
	// for Load. Set by TestMain after the binary location is
	// resolved.
	kprobeObjectPath string
)

func TestMain(m *testing.M) {
	cleanup, err := bootstrap()

	// Cleanup may need to run from two paths: the normal end of
	// TestMain, or a signal handler that fires when the user
	// SIGINTs the test before m.Run returns. sync.Once makes the
	// second call a no-op.
	var once sync.Once
	runCleanup := func() {
		if cleanup != nil {
			once.Do(cleanup)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "caught %s, shutting daemon down\n", sig)
		runCleanup()
		exitCode := 130 // 128 + SIGINT
		if sig == syscall.SIGTERM {
			exitCode = 143
		}
		os.Exit(exitCode)
	}()

	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		runCleanup()
		os.Exit(1)
	}
	code := m.Run()
	runCleanup()
	os.Exit(code)
}

func bootstrap() (func(), error) {
	binary, err := resolveBpfmanBinary()
	if err != nil {
		return nil, fmt.Errorf("resolve binary: %w", err)
	}
	object, err := resolveKprobeObject()
	if err != nil {
		return nil, fmt.Errorf("resolve kprobe object: %w", err)
	}

	tmpRoot, err := os.MkdirTemp("", "bpfman-grpc-parallel-")
	if err != nil {
		return nil, fmt.Errorf("tmp root: %w", err)
	}
	cleanupRoot := func() { _ = os.RemoveAll(tmpRoot) }

	runtimeDir := filepath.Join(tmpRoot, "runtime")
	cacheDir := filepath.Join(tmpRoot, "cache")
	socketPath := filepath.Join(tmpRoot, "bpfman.sock")
	for _, d := range []string{runtimeDir, cacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			cleanupRoot()
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	serverCmd := exec.CommandContext(ctx, binary, "serve",
		"--runtime-dir", runtimeDir,
		"--image-cache-dir", cacheDir,
		"--socket-path", socketPath,
		// Disable the default TCP listener; the test only
		// talks to the daemon over the per-run UDS, and a
		// fixed TCP port would collide across concurrent
		// runs or with an orphaned daemon from a previous
		// run.
		"--tcp-address=",
	)
	serverCmd.Stdout = os.Stderr
	serverCmd.Stderr = os.Stderr
	serverCmd.SysProcAttr = &syscall.SysProcAttr{
		// Own process group so a stray ctx-cancel SIGKILL
		// does not also reach this test binary.
		Setpgid: true,
		// If the test binary dies for any reason -- Ctrl+C
		// before deferred cleanup runs, panic, SIGKILL from
		// the test runner -- the kernel sends the daemon
		// SIGKILL. Last line of defence against leaving a
		// daemon listening on the UDS path between runs.
		Pdeathsig: syscall.SIGKILL,
	}

	if err := serverCmd.Start(); err != nil {
		cancel()
		cleanupRoot()
		return nil, fmt.Errorf("start daemon: %w", err)
	}

	cleanupDaemon := func() {
		// Prefer graceful shutdown. If the daemon does not
		// exit promptly the deferred cancel below will SIGKILL
		// it on the way out.
		_ = syscall.Kill(serverCmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = serverCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
		}
		cancel()
		cleanupRoot()
	}

	if err := waitForSocket(socketPath, 10*time.Second); err != nil {
		cleanupDaemon()
		return nil, fmt.Errorf("wait for socket: %w", err)
	}

	conn, err := grpc.NewClient("unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cleanupDaemon()
		return nil, fmt.Errorf("dial: %w", err)
	}
	client = pb.NewBpfmanClient(conn)
	kprobeObjectPath = object

	return func() {
		_ = conn.Close()
		cleanupDaemon()
	}, nil
}

// TestKprobeParallel_GRPC fans N goroutines through the full kprobe
// lifecycle against the daemon. Each iteration is an independent
// Load -> Get -> Attach -> GetLink -> ListLinks -> Detach -> Unload.
// No counter-delta assertion: that lives in the .bpfman scripts,
// which encode the exact-equality contract. Here we care about
// lifecycle correctness under concurrency: handler flock
// acquisition under contention, lockless reads racing with writer
// mutators, and per-program-id allocation behaving distinctly
// across goroutines.
func TestKprobeParallel_GRPC(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (bpfman load)")
	}

	n := envInt("BPFMAN_GRPC_PARALLEL_N", 32)
	iters := envInt("BPFMAN_GRPC_PARALLEL_ITERS", 4)

	var wg sync.WaitGroup
	errCh := make(chan error, n*iters)

	for g := 0; g < n; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if err := runOneLifecycle(t.Context(), gid, i); err != nil {
					errCh <- fmt.Errorf("goroutine %d iter %d: %w", gid, i, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func runOneLifecycle(ctx context.Context, gid, iter int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// ---- Load ----
	loadResp, err := client.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: kprobeObjectPath},
		},
		Info: []*pb.LoadInfo{{
			Name:        "kprobe_counter",
			ProgramType: pb.BpfmanProgramType_KPROBE,
		}},
		Metadata: map[string]string{
			"test":      "grpc_parallel",
			"goroutine": strconv.Itoa(gid),
			"iter":      strconv.Itoa(iter),
		},
	})
	if err != nil {
		return fmt.Errorf("Load: %w", err)
	}
	if len(loadResp.Programs) != 1 {
		return fmt.Errorf("Load: want 1 program, got %d", len(loadResp.Programs))
	}
	if loadResp.Programs[0].KernelInfo == nil {
		return errors.New("Load: missing KernelInfo")
	}
	progID := loadResp.Programs[0].KernelInfo.Id

	// ---- Get round-trip ----
	getResp, err := client.Get(ctx, &pb.GetRequest{Id: progID})
	if err != nil {
		return fmt.Errorf("Get %d: %w", progID, err)
	}
	if getResp.KernelInfo == nil || getResp.KernelInfo.Id != progID {
		return fmt.Errorf("Get %d: id mismatch", progID)
	}
	if got := getResp.Info.Metadata["goroutine"]; got != strconv.Itoa(gid) {
		return fmt.Errorf("Get %d: metadata.goroutine %q != %d", progID, got, gid)
	}

	// ---- Attach ----
	attachResp, err := client.Attach(ctx, &pb.AttachRequest{
		Id: progID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_KprobeAttachInfo{
				KprobeAttachInfo: &pb.KprobeAttachInfo{
					FnName: "do_unlinkat",
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("Attach %d: %w", progID, err)
	}
	linkID := attachResp.LinkId

	// ---- GetLink round-trip ----
	getLinkResp, err := client.GetLink(ctx, &pb.GetLinkRequest{KernelLinkId: linkID})
	if err != nil {
		return fmt.Errorf("GetLink %d: %w", linkID, err)
	}
	if getLinkResp.Link == nil || getLinkResp.Link.Summary == nil ||
		getLinkResp.Link.Summary.KernelLinkId != linkID {
		return fmt.Errorf("GetLink %d: link id mismatch", linkID)
	}

	// ---- ListLinks filtered to our program should include our link ----
	listResp, err := client.ListLinks(ctx, &pb.ListLinksRequest{ProgramId: &progID})
	if err != nil {
		return fmt.Errorf("ListLinks for program %d: %w", progID, err)
	}
	found := false
	for _, l := range listResp.Links {
		if l.Summary != nil && l.Summary.KernelLinkId == linkID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("ListLinks: link %d missing for program %d", linkID, progID)
	}

	// ---- Detach ----
	if _, err := client.Detach(ctx, &pb.DetachRequest{LinkId: linkID}); err != nil {
		return fmt.Errorf("Detach %d: %w", linkID, err)
	}
	if _, err := client.GetLink(ctx, &pb.GetLinkRequest{KernelLinkId: linkID}); err == nil {
		return fmt.Errorf("post-Detach: GetLink %d still succeeds", linkID)
	}

	// ---- Unload ----
	if _, err := client.Unload(ctx, &pb.UnloadRequest{Id: progID}); err != nil {
		return fmt.Errorf("Unload %d: %w", progID, err)
	}
	if _, err := client.Get(ctx, &pb.GetRequest{Id: progID}); err == nil {
		return fmt.Errorf("post-Unload: Get %d still succeeds", progID)
	}

	return nil
}

// resolveBpfmanBinary returns the path to the bpfman binary,
// honouring $BPFMAN_BIN first then defaulting to <repo>/bin/bpfman.
func resolveBpfmanBinary() (string, error) {
	if env := os.Getenv("BPFMAN_BIN"); env != "" {
		return env, nil
	}
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "bin", "bpfman")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%s: %w (build it with `make`)", p, err)
	}
	return p, nil
}

// resolveKprobeObject returns the absolute path to
// e2e/testdata/bpf/kprobe_counter.bpf.o under the repo root.
func resolveKprobeObject() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "e2e", "testdata", "bpf", "kprobe_counter.bpf.o")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%s: %w (build with `make`)", p, err)
	}
	return p, nil
}

// repoRoot walks up from this source file's directory until it
// finds a go.mod.
func repoRoot() (string, error) {
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(this)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", filepath.Dir(this))
		}
		dir = parent
	}
}

// waitForSocket polls until the daemon socket accepts a connect or
// the timeout expires.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon socket %s never became ready: %w", path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func envInt(name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
