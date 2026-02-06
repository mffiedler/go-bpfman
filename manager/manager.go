// Package manager provides high-level orchestration using
// the fetch/compute/execute pattern.
//
// # Atomic Load Model
//
// The Manager provides atomic semantics for loading BPF programs.
// The goal is to ensure that either a program is fully loaded with its
// metadata persisted, or nothing is left behind (no partial state).
//
// The atomic model:
//  1. Load program into kernel and pin to bpffs
//  2. On success: persist metadata to DB in a single transaction
//  3. On failure: cleanup kernel state, nothing in DB
//  4. GC handles orphans from crashes
//
// This is simpler than the previous 2PC reservation pattern because:
//   - Programs only exist in DB after successful load
//   - No "loading" or "error" states to manage
//   - GC only needs to handle orphan pins (crash recovery)
//
// # CSI Integration
//
// The CSI driver is a consumer of loaded programs, not part of the
// transaction. It creates per-pod views of maps via re-pinning:
//
//	canonical: /sys/fs/bpf/bpfman/<kernel_id>/<map>     (managed by bpfman)
//	per-pod:   /run/bpfman/csi/fs/<vol>/<map>          (per-pod bpffs mount)
//
// The per-pod path is a separate bpffs mount. Re-pinning creates a new
// pin from the map's file descriptor - this is not a rename across
// filesystems, so there are no cross-device issues.
//
// CSI cleanup removes the per-pod bpffs mount; canonical pins are
// unaffected and remain managed by bpfman.
package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/outcome"
)

// opIDKey is the context key for operation IDs.
type opIDKey struct{}

// ContextWithOpID returns a new context with the given operation ID.
func ContextWithOpID(ctx context.Context, opID uint64) context.Context {
	return context.WithValue(ctx, opIDKey{}, opID)
}

// OpIDFromContext extracts the operation ID from context, or returns 0 if not set.
func OpIDFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(opIDKey{}); v != nil {
		return v.(uint64)
	}
	return 0
}

// Manager orchestrates BPF program management using fetch/compute/execute.
type Manager struct {
	root              bpfmanfs.Root
	store             interpreter.Store
	kernel            interpreter.KernelOperations
	executor          interpreter.ActionExecutor
	programDiscoverer interpreter.ProgramDiscoverer
	logger            *slog.Logger

	// GC coordination - separate from request-level locking
	gcMu           sync.Mutex
	mutatedSinceGC bool
}

// New creates a new Manager with all required dependencies.
//
// All parameters are required:
//   - root: filesystem root for runtime directories
//   - store: database for program/link metadata
//   - kernel: kernel operations adapter
//   - programDiscoverer: discovers existing kernel programs
//   - mounter: handles bpffs mounting (use RealMounter for production, NoOpMounter for tests)
//   - logger: structured logger (nil uses slog.Default())
//
// New ensures runtime directories exist and bpffs is mounted via the
// provided mounter. For tests, use NoOpMounter to skip actual mounting.
//
// The logger should already be wrapped with WithOpIDHandler by the caller
// (typically the server) to enable op_id extraction from context.
func New(root bpfmanfs.Root, store interpreter.Store, kernel interpreter.KernelOperations, programDiscoverer interpreter.ProgramDiscoverer, mounter BPFFSMounter, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	setupLogger := logger.With("component", "setup")

	// Ensure runtime directories exist
	setupLogger.Debug("ensuring runtime directories",
		"base", root.Base(),
		"fs", root.BPFFSMountPoint(),
		"db", root.DBPath())

	for _, dir := range root.RuntimeDirs() {
		if err := os.MkdirAll(dir, 0755); err != nil {
			setupLogger.Error("failed to create directory", "dir", dir, "error", err)
			return nil, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	// Mount bpffs via the provided mounter
	if err := mounter.EnsureMounted(root.BPFFSMountPoint()); err != nil {
		setupLogger.Error("failed to mount bpffs", "error", err)
		return nil, err
	}
	setupLogger.Debug("runtime directories ready")

	return &Manager{
		root:              root,
		store:             store,
		kernel:            kernel,
		programDiscoverer: programDiscoverer,
		executor:          interpreter.NewExecutor(store, kernel),
		logger:            logger.With("component", "manager"),
		mutatedSinceGC:    true, // Force GC on first operation
	}, nil
}

// Root returns the filesystem root.
func (m *Manager) Root() bpfmanfs.Root {
	return m.root
}

// GCResult contains statistics and outcome from garbage collection.
type GCResult struct {
	// Statistics from GC.
	ProgramsRemoved    int
	DispatchersRemoved int
	LinksRemoved       int
	OrphanPinsRemoved  int
	// LiveOrphans counts programs pinned under bpfman's bpffs root
	// that are still alive in the kernel but have no DB record.
	LiveOrphans int

	// Outcome tracks the structured result of the GC operation.
	Outcome outcome.OperationOutcome
}

// GCOptions configures garbage collection behaviour.
type GCOptions struct {
	// Rules restricts GC to the named rules. If empty, all rules run.
	Rules []string
	// Prune removes live orphans (programs pinned under bpfman's
	// bpffs root that are still alive in the kernel but have no DB
	// record). Without prune, these are counted but left untouched.
	Prune bool
}

// GC removes stale database entries that no longer exist in the kernel.
// This should be called at startup before accepting requests. After GC,
// the database is authoritative for the session.
//
// Stale entries can occur when:
//   - The daemon restarts but kernel state was lost (e.g., system reboot)
//   - A previous unload operation failed partway through
//   - External tools removed BPF objects without updating the database
//   - The kernel reused a program ID after unload
//
// A DB program is only preserved if both the kernel ID is live and our
// bpffs pin path exists. This prevents stale DB rows from surviving
// when the kernel reuses an ID that now belongs to a different program.
// GC runs garbage collection with all rules.
func (m *Manager) GC(ctx context.Context) (GCResult, error) {
	return m.GCWithOptions(ctx, GCOptions{})
}

// GCWithRules runs garbage collection. If rules is non-empty, only the
// specified GC rules are run; otherwise all rules are run. Store-level
// GC always runs regardless of the rules filter.
func (m *Manager) GCWithRules(ctx context.Context, rules []string) (GCResult, error) {
	return m.GCWithOptions(ctx, GCOptions{Rules: rules})
}

// GCWithOptions runs garbage collection with the given options.
func (m *Manager) GCWithOptions(ctx context.Context, opts GCOptions) (result GCResult, retErr error) {
	rec := outcome.NewRecorder(&result.Outcome)
	defer func() { rec.Finalise() }()
	result.Outcome.OpID = OpIDFromContext(ctx)
	start := time.Now()

	// Gather kernel state
	kernelProgramIDs := make(map[uint32]bool)
	for kp, err := range m.kernel.Programs(ctx) {
		if err != nil {
			m.logger.WarnContext(ctx, "error iterating kernel programs", "error", err)
			continue
		}
		kernelProgramIDs[kp.ID] = true
	}

	// For any DB program whose kernel ID is still live, verify
	// that our bpffs pin exists. If the pin is gone the kernel
	// ID may have been recycled to a program that is not ours;
	// remove it from the live set so the store GC reaps the row.
	dbPrograms, err := m.store.List(ctx)
	if err != nil {
		retErr = fmt.Errorf("list programs: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: "store",
			Error:  retErr.Error(),
		})
		result.Outcome.PrimaryError = retErr.Error()
		return
	}
	scanner := m.root.BPFFS().Scanner()
	for id := range dbPrograms {
		if !kernelProgramIDs[id] {
			continue // already absent; store GC will reap
		}
		pinPath := m.root.BPFFS().ProgPinPath(id)
		if !scanner.PathExists(pinPath) {
			m.logger.InfoContext(ctx, "pin missing for live kernel ID, marking for reap",
				"kernel_id", id, "pin_path", pinPath)
			delete(kernelProgramIDs, id)
		}
	}

	kernelLinkIDs := make(map[uint32]bool)
	for kl, err := range m.kernel.Links(ctx) {
		if err != nil {
			m.logger.WarnContext(ctx, "error iterating kernel links", "error", err)
			continue
		}
		kernelLinkIDs[kl.ID] = true
	}

	// Phase 1: Delegate to store - it handles ordering constraints internally
	storeResult, err := m.store.GC(ctx, kernelProgramIDs, kernelLinkIDs)
	if err != nil {
		retErr = fmt.Errorf("store gc: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreGCPrograms,
			Target: "store",
			Error:  retErr.Error(),
		})
		result.Outcome.PrimaryError = retErr.Error()
		return
	}

	// Record Phase 1 steps
	result.ProgramsRemoved = storeResult.ProgramsRemoved
	result.LinksRemoved = storeResult.LinksRemoved
	result.DispatchersRemoved = storeResult.DispatchersRemoved

	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreGCPrograms,
		Target: "store",
		Details: outcome.GCPhaseDetails{
			Removed: storeResult.ProgramsRemoved,
		},
	})
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreGCLinks,
		Target: "store",
		Details: outcome.GCPhaseDetails{
			Removed: storeResult.LinksRemoved,
		},
	})
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreGCDispatchers,
		Target: "store",
		Details: outcome.GCPhaseDetails{
			Removed: storeResult.DispatchersRemoved,
		},
	})

	// Phase 2: Post-store GC using the coherency rule engine to detect and
	// remove stale dispatchers and orphan filesystem artefacts.
	state, err := GatherState(ctx, m.store, m.kernel, m.root)
	if err != nil {
		m.logger.WarnContext(ctx, "failed to gather state for post-store GC", "error", err)
	} else {
		// Build rule set: standard GC rules, plus prune rule if requested.
		gcRules := GCRules()
		if opts.Prune {
			gcRules = append(gcRules, PruneRule())
		}
		if len(opts.Rules) > 0 {
			ruleSet := make(map[string]bool)
			for _, r := range opts.Rules {
				ruleSet[r] = true
			}
			filtered := gcRules[:0]
			for _, r := range gcRules {
				if ruleSet[r.Name] {
					filtered = append(filtered, r)
				}
			}
			gcRules = filtered
		}

		violations := Evaluate(state, gcRules)
		for _, v := range violations {
			if v.Op == nil {
				continue
			}
			if err := v.Op.Execute(); err != nil {
				m.logger.WarnContext(ctx, "gc operation failed", "op", v.Op.Description, "error", err)
				// Record failed orphan removal step
				_ = rec.Fail(outcome.Step{
					Kind:   outcome.StepKindGCRemoveOrphan,
					Target: v.Op.Description,
					Details: outcome.OrphanDetails{
						Category: v.Category,
					},
					Error: err.Error(),
				})
				retErr = fmt.Errorf("gc operation failed: %s: %w", v.Op.Description, err)
				result.Outcome.PrimaryError = retErr.Error()
				// Continue to attempt other cleanup operations but mark overall as failed
				continue
			}
			m.logger.InfoContext(ctx, "gc operation applied", "op", v.Op.Description)
			_ = rec.Complete(outcome.Step{
				Kind:   outcome.StepKindGCRemoveOrphan,
				Target: v.Op.Description,
				Details: outcome.OrphanDetails{
					Category: v.Category,
				},
			})
			switch v.Category {
			case "gc-dispatcher":
				result.DispatchersRemoved++
			case "gc-orphan-pin":
				result.OrphanPinsRemoved++
			}
		}

		// Count live orphans: orphan pins where kernel is alive.
		// When prune is set, these were already handled above.
		if !opts.Prune {
			result.LiveOrphans = state.LiveOrphans()
		}
	}

	elapsed := time.Since(start)
	if result.ProgramsRemoved > 0 || result.DispatchersRemoved > 0 || result.LinksRemoved > 0 || result.OrphanPinsRemoved > 0 {
		m.logger.InfoContext(ctx, "gc complete",
			"duration", elapsed,
			"programs_removed", result.ProgramsRemoved,
			"dispatchers_removed", result.DispatchersRemoved,
			"links_removed", result.LinksRemoved,
			"orphan_pins_removed", result.OrphanPinsRemoved,
			"live_orphans", result.LiveOrphans)
	} else if result.LiveOrphans > 0 {
		m.logger.InfoContext(ctx, "gc complete",
			"duration", elapsed,
			"live_orphans", result.LiveOrphans)
	} else {
		m.logger.DebugContext(ctx, "gc complete", "duration", elapsed)
	}

	return
}

// GCIfNeeded runs GC if required, with its own mutex for coordination.
// For mutating operations, always runs GC. For read operations, only runs
// GC if a mutating operation occurred since the last GC.
// This allows concurrent readers at the server level while serialising GC.
func (m *Manager) GCIfNeeded(ctx context.Context, mutating bool) error {
	m.gcMu.Lock()
	defer m.gcMu.Unlock()

	if !mutating && !m.mutatedSinceGC {
		return nil // Read op and no mutations since last GC - skip
	}

	if _, err := m.GC(ctx); err != nil {
		return err
	}
	m.mutatedSinceGC = false
	return nil
}

// MarkMutated records that a mutating operation occurred.
// Call this after successful mutating operations (Load, Unload, Attach, Detach).
func (m *Manager) MarkMutated() {
	m.gcMu.Lock()
	m.mutatedSinceGC = true
	m.gcMu.Unlock()
}
