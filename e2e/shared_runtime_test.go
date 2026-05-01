//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/frobware/go-bpfman/fs"
	fsruntime "github.com/frobware/go-bpfman/fs/runtime"
	"github.com/frobware/go-bpfman/logging"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/image/oci"
	"github.com/frobware/go-bpfman/platform/image/verify"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
)

// e2eSharedRuntimeEnv selects the production-shaped runtime mode:
// every test runs against one bpffs mount, one sqlite store, and one
// manager instance.  The default (env unset) keeps the per-test
// isolated runtime.  Shared mode exists to surface concurrency,
// resource-tracking, and cross-tenant interactions that the isolated
// per-test path papers over.  See docs/SHARED-RUNTIME.md (planned)
// for the full rationale.
const e2eSharedRuntimeEnv = "BPFMAN_E2E_SHARED_RUNTIME"

// e2eSuiteRootEnv overrides the default suite root path (used in
// shared mode) so debug paths can be predictable across runs.
const e2eSuiteRootEnv = "BPFMAN_E2E_SUITE_ROOT"

// sharedRuntimeMode reports whether tests should reuse a single
// process-wide runtime instead of building one per test.
func sharedRuntimeMode() bool {
	return os.Getenv(e2eSharedRuntimeEnv) == "1"
}

// suiteRuntime is the singleton runtime stood up by TestMain when
// shared mode is active.  Tests read it via NewTestEnv; the package
// is not the access boundary -- tests still reach it through TestEnv
// so the rest of the helpers can stay mode-agnostic.
type suiteRuntime struct {
	layout      fs.Layout
	manager     *manager.Manager
	imagePuller platform.ImagePuller
	logger      *slog.Logger
	baseDir     string
	closeStore  func() error
}

var (
	sharedRuntime     *suiteRuntime
	sharedRuntimeOnce sync.Once
	sharedRuntimeErr  error
)

// initSharedRuntime stands up the suite-wide runtime.  Idempotent:
// the first call performs setup, subsequent calls return the cached
// instance.  Caller is TestMain; tests just observe sharedRuntime.
func initSharedRuntime() (*suiteRuntime, error) {
	sharedRuntimeOnce.Do(func() {
		sharedRuntime, sharedRuntimeErr = buildSuiteRuntime()
	})
	return sharedRuntime, sharedRuntimeErr
}

func buildSuiteRuntime() (*suiteRuntime, error) {
	baseDir := os.Getenv(e2eSuiteRootEnv)
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), fmt.Sprintf("bpfman-e2e-suite-%d", os.Getpid()))
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create suite root %s: %w", baseDir, err)
	}

	if err := materialiseBPFFS(baseDir); err != nil {
		return nil, fmt.Errorf("materialise embedded BPF objects under %s: %w", baseDir, err)
	}

	layout, err := fs.New(baseDir)
	if err != nil {
		return nil, fmt.Errorf("invalid suite layout: %w", err)
	}

	imageCacheBase, err := fs.NewImageCache(filepath.Join(layout.Base(), "cache", "image"))
	if err != nil {
		return nil, fmt.Errorf("invalid image cache directory: %w", err)
	}
	imageCache, err := fs.EnsureCache(imageCacheBase)
	if err != nil {
		return nil, fmt.Errorf("ensure image cache: %w", err)
	}

	logger, err := buildLogger()
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	store, err := sqlite.New(ctx, layout.DBPath(), logger)
	if err != nil {
		return nil, fmt.Errorf("create suite store: %w", err)
	}

	kernel := ebpf.New(ebpf.WithLogger(logger))

	ensuredRuntime, err := fsruntime.New(layout, fsruntime.RealMounter{}, logger)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("ensure suite runtime: %w", err)
	}

	verifier := verify.NoSign()
	puller, err := oci.NewPuller(
		imageCache,
		oci.WithLogger(logger),
		oci.WithVerifier(verifier),
	)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("create image puller: %w", err)
	}

	mgr, err := manager.New(ensuredRuntime, puller, store, kernel, ebpf.NewProgramDiscoverer(), logger)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("create suite manager: %w", err)
	}

	logger.Info("shared e2e runtime ready", "base", baseDir, "bpffs", layout.BPFFSMountPoint())

	return &suiteRuntime{
		layout:      layout,
		manager:     mgr,
		imagePuller: puller,
		logger:      logger,
		baseDir:     baseDir,
		closeStore:  store.Close,
	}, nil
}

// teardownSharedRuntime asserts the suite finished clean, closes
// the store, unmounts the bpffs, and removes the suite root.
// Returns true if any leak was detected so TestMain can promote a
// passing exit code to a failure -- if tests passed individually
// but the suite as a whole left programs or links behind, that's a
// real bug and worth surfacing as a non-zero exit.  Cleanup
// failures (close, unmount, rm) are logged but do not promote the
// exit code: they are operational, not behavioural, and shouldn't
// drown out the leak signal we actually care about.
func teardownSharedRuntime(rt *suiteRuntime) (leaked bool) {
	if rt == nil {
		return false
	}

	leaked = assertSuiteCleanState(rt)

	if rt.closeStore != nil {
		if err := rt.closeStore(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e suite teardown: close store: %v\n", err)
		}
	}
	bpffsMount := rt.layout.BPFFSMountPoint()
	if err := unmount(bpffsMount); err != nil {
		fmt.Fprintf(os.Stderr, "e2e suite teardown: unmount %s: %v\n", bpffsMount, err)
	}
	if err := os.RemoveAll(rt.baseDir); err != nil {
		fmt.Fprintf(os.Stderr, "e2e suite teardown: remove %s: %v\n", rt.baseDir, err)
	}
	return leaked
}

// assertSuiteCleanState lists the manager's residual state at suite
// end and reports any leaked programs or links to stderr.  Returns
// true if anything leaked.  This is the final safety net of shared
// mode: every test's t.Cleanup is supposed to detach links and
// unload programs before the test returns, so post-suite the
// manager should be empty.  Anything left over is a bug -- either a
// missing t.Cleanup, a Detach/Unload that didn't actually persist,
// or a manager-side leak.
func assertSuiteCleanState(rt *suiteRuntime) bool {
	ctx := context.Background()
	leaked := false

	progResult, err := rt.manager.ListPrograms(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e suite teardown: list programs: %v\n", err)
	} else if len(progResult.Programs) > 0 {
		leaked = true
		fmt.Fprintf(os.Stderr, "e2e suite teardown: %d program(s) leaked at suite end:\n", len(progResult.Programs))
		for _, p := range progResult.Programs {
			id := uint32(0)
			if p.Status.Kernel != nil {
				id = uint32(p.Status.Kernel.ID)
			}
			fmt.Fprintf(os.Stderr, "  prog id=%d name=%q metadata=%v\n",
				id, p.Record.Meta.Name, p.Record.Meta.Metadata)
		}
	}

	links, err := rt.manager.ListLinks(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e suite teardown: list links: %v\n", err)
	} else if len(links) > 0 {
		leaked = true
		fmt.Fprintf(os.Stderr, "e2e suite teardown: %d link(s) leaked at suite end:\n", len(links))
		for _, l := range links {
			fmt.Fprintf(os.Stderr, "  link id=%d kind=%s prog=%d\n",
				l.ID, l.Kind, l.ProgramID)
		}
	}

	return leaked
}

// buildLogger composes the slog handler the same way NewTestEnv
// does, so per-test and shared modes log identically when
// BPFMAN_LOG is set.
func buildLogger() (*slog.Logger, error) {
	if envSpec := os.Getenv("BPFMAN_LOG"); envSpec != "" {
		l, err := logging.New(logging.Options{
			EnvSpec: envSpec,
			Format:  logging.FormatText,
			Output:  os.Stderr,
		})
		if err != nil {
			return nil, fmt.Errorf("invalid BPFMAN_LOG spec: %w", err)
		}
		return l, nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	})), nil
}

// requireSharedRuntimeForTest pulls the shared runtime out for use
// by NewTestEnv when sharedRuntimeMode() is true.  Tests should
// never call this directly; it exists as the boundary helpers.go
// crosses to wire a TestEnv against the singleton.
func requireSharedRuntimeForTest(t *testing.T) *suiteRuntime {
	t.Helper()
	if sharedRuntime == nil {
		t.Fatalf("shared runtime requested but not initialised; TestMain must call initSharedRuntime when %s=1", e2eSharedRuntimeEnv)
	}
	return sharedRuntime
}
