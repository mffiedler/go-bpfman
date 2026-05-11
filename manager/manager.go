package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/coherency"
	"github.com/frobware/go-bpfman/platform"
)

// opIDKey is the context key for operation IDs.
type opIDKey struct{}

// opActiveKey is the context key that marks a manager operation as
// active. When present, gcOnEntry skips GC because coherence was
// already established by the outermost caller.
type opActiveKey struct{}

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
	rt                fs.Runtime
	store             platform.Store
	kernel            platform.KernelOperations
	executor          action.ExecutorWithResult
	programDiscoverer platform.ProgramDiscoverer
	imagePuller       platform.ImagePuller // optional, nil if not configured
	logger            *slog.Logger

	// Diagnostic: per-(iface, direction) nsid that this manager
	// has captured for prior TCX attaches. Used to detect when a
	// new attach computes a different nsid than its siblings on
	// the same interface, which indicates the calling OS thread
	// is in the wrong netns and bpfman's per-thread nsid capture
	// has been corrupted by an upstream thread-leak. Loaded /
	// stored from manager/attach_tc.go.
	tcxIfaceNsids sync.Map
}

// New creates a new Manager with all required dependencies.
//
// Required parameters:
//   - rt: runtime capability token (from runtime.New()) proving directories and bpffs are ready
//   - store: database for program/link metadata
//   - kernel: kernel operations adapter
//   - programDiscoverer: discovers existing kernel programs
//   - logger: structured logger (nil uses slog.Default())
//
// Optional parameters:
//   - imagePuller: OCI image puller for loading programs from container images (nil to disable)
//
// The rt parameter is a capability token from fs/runtime.New()
// that proves the filesystem directories exist and bpffs is mounted.
//
// The logger should already be wrapped with WithOpIDHandler by the caller
// (typically the server) to enable op_id extraction from context.
func New(
	rt fs.Runtime,
	imagePuller platform.ImagePuller,
	store platform.Store,
	kernel platform.KernelOperations,
	programDiscoverer platform.ProgramDiscoverer,
	logger *slog.Logger,
) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		rt:                rt,
		store:             store,
		kernel:            kernel,
		programDiscoverer: programDiscoverer,
		imagePuller:       imagePuller,
		executor:          newExecutor(store, kernel, rt.Bytecode(), rt.BPFFS(), logger).(action.ExecutorWithResult),
		logger:            logger.With("component", "manager"),
	}, nil
}

// Layout returns the filesystem layout.
func (m *Manager) Layout() fs.Layout {
	return m.rt.Layout()
}

// Runtime returns the filesystem runtime capability token.
func (m *Manager) Runtime() fs.Runtime {
	return m.rt
}

// ImagePuller returns the image puller, or nil if not configured.
func (m *Manager) ImagePuller() platform.ImagePuller {
	return m.imagePuller
}

// GCResult contains statistics from garbage collection.
type GCResult struct {
	ProgramsRemoved    int
	DispatchersRemoved int
	LinksRemoved       int
	OrphanPinsRemoved  int
	// LiveOrphans counts programs pinned under bpfman's bpffs root
	// that are still alive in the kernel but have no DB record.
	LiveOrphans int
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

// GC removes stale database entries that no longer exist in the
// kernel. Every mutating manager method (Load, Unload, Attach,
// Detach) runs GC on the way in via gcOnEntry, so callers do not
// normally need to invoke this explicitly. Operators can run GC
// directly via `bpfman gc`.
//
// Stale entries can occur when:
//   - A previous unload operation failed partway through.
//   - External tools removed BPF objects without updating the
//     database.
//   - The kernel reused a program ID after unload.
//   - Persistent state from a prior process is reconciled on the
//     first mutation of a new process.
//
// A DB program is only preserved if both the kernel ID is live and
// our bpffs pin path exists. This prevents stale DB rows from
// surviving when the kernel reuses an ID that now belongs to a
// different program.
//
// computeStoreGC's deletion phases are gated on kernel-enumeration
// completeness inside ComputeGC; under partial enumeration the
// store-deletion phases are skipped to avoid misclassifying healthy
// state as stale.
func (m *Manager) GC(ctx context.Context, writeLock lock.WriterScope) (GCResult, error) {
	return m.GCWithOptions(ctx, writeLock, GCOptions{})
}

// GCWithRules runs garbage collection. If rules is non-empty, only the
// specified GC rules are run; otherwise all rules are run. Store-level
// GC always runs regardless of the rules filter.
func (m *Manager) GCWithRules(ctx context.Context, writeLock lock.WriterScope, rules []string) (GCResult, error) {
	return m.GCWithOptions(ctx, writeLock, GCOptions{Rules: rules})
}

// GCPlan holds the computed GC actions before execution. In dry-run
// mode the plan is returned without executing.
type GCPlan struct {
	// StoreActions are deletions computed by computeStoreGC.
	StoreActions []action.Action
	// Violations are coherency-level GC violations with
	// remediation operations (only those with Op != nil).
	Violations []coherency.Violation
	// LiveOrphans counts programs pinned under bpfman's bpffs root
	// that are alive in the kernel but have no DB record.
	LiveOrphans int
}

// IsEmpty reports whether the plan has no remediation work to do.
// A plan is empty when there are no store actions and no coherency
// violations whose Intent is non-nil. Violations without an Intent
// are diagnostic-only and do not constitute remediation work; they
// surface in audit reports but do not warrant taking the writer
// lock.
//
// gcOnEntry's lockless pre-check uses this to decide whether the
// lock acquisition is worth paying for: on an empty plan the
// caller can skip the lock entirely.
func (p GCPlan) IsEmpty() bool {
	if len(p.StoreActions) > 0 {
		return false
	}
	for _, v := range p.Violations {
		if v.Intent != nil {
			return false
		}
	}
	return true
}

// errRollback is a sentinel used to force a transaction rollback
// without indicating a real failure.
var errRollback = errors.New("rollback")

// ComputeGC is the legacy entry point that takes a WriterScope as a
// proof-of-lock-held sentinel. The body delegates to GCScan, which
// is safe to call without the lock; callers that hold the writer
// lock are free to use either method.
//
// New code should prefer GCScan (lockless) followed by GCRemediate
// (under lock, only when GCScan returns non-empty). See
// docs/PLAN-load-lock-tightening.md.
func (m *Manager) ComputeGC(ctx context.Context, writeLock lock.WriterScope, opts GCOptions) (GCPlan, error) {
	_ = writeLock
	return m.GCScan(ctx, opts)
}

// GCScan gathers state and computes what GC would do, without
// executing any actions and without holding the global writer
// lock. The returned GCPlan can be inspected for dry-run reporting
// or fed into GCRemediate / ExecuteGC for execution.
//
// State is gathered once via GatherState (which includes a full
// inspect.Snapshot). The store GC inputs are derived from the
// resulting Observation, avoiding the redundant kernel and store
// enumeration that previously occurred between direct queries and
// the Snapshot inside evaluateCoherency.
//
// When there are store actions, they are applied inside a sqlite
// transaction that is then rolled back, so that coherency rules
// evaluate against the post-deletion state and the plan reflects
// the full set of operations remediation would perform. The
// rolled-back transaction touches sqlite only; the global writer
// lock is not required because sqlite manages its own
// serialisation and no kernel state is modified.
//
// Callers that intend to remediate must follow this with
// GCRemediate (which re-scans under the lock for the authoritative
// set; the plan returned here is a hint, not a contract).
func (m *Manager) GCScan(ctx context.Context, opts GCOptions) (GCPlan, error) {
	var plan GCPlan

	// Single gather pass for both store GC and coherency.
	state, err := coherency.GatherState(ctx, m.store, m.kernel, m.Layout())
	if err != nil {
		return plan, fmt.Errorf("gather state: %w", err)
	}

	// Derive store GC inputs from the correlated observation.
	obs := state.Observation()
	in := deriveStoreGCInputs(obs)

	// Log pin-missing programs for visibility. A store-managed
	// program with a live kernel ID but no pin suggests the ID
	// was recycled; the program will be reaped.
	for _, p := range obs.Programs {
		if p.Presence.InStore && p.Presence.InKernel && !in.kernelPrograms[p.ProgramID] {
			m.logger.InfoContext(ctx, "pin missing for live kernel ID, marking for reap",
				"program_id", p.ProgramID, "pin_path", p.PinPath())
		}
	}

	// Gate computeStoreGC on kernel-enumeration completeness. Phase 3
	// of computeStoreGC (manager/gc.go:74) deletes any non-synthetic,
	// non-XDP/TC store link whose ID is missing from the kernel link
	// set. Under partial enumeration, healthy links look stale and
	// would be incorrectly deleted; the next mutating call on those
	// links would then surface ErrLinkNotManaged. Same hazard applies
	// to program-side phases. The audit rule
	// kernel-enumeration-incomplete (manager/coherency/rules.go:55)
	// continues to surface this state for operator visibility.
	if obs.Meta.ProgramEnumErrors == 0 && obs.Meta.LinkEnumErrors == 0 {
		plan.StoreActions = computeStoreGC(in.programs, in.dispatchers, in.links, in.kernelPrograms, in.kernelLinks)
	} else {
		m.logger.WarnContext(ctx, "skipping store-GC: kernel enumeration incomplete",
			"program_enum_errors", obs.Meta.ProgramEnumErrors,
			"link_enum_errors", obs.Meta.LinkEnumErrors)
	}

	// Coherency rule engine. When there are store actions, apply
	// them inside a rolled-back transaction and re-gather coherency
	// state so the rules see the post-deletion observation. When there
	// are no store actions, reuse the state we already gathered.
	if len(plan.StoreActions) > 0 {
		txErr := m.store.RunInTransaction(ctx, func(tx platform.Store) error {
			txExec := newExecutor(tx, m.kernel, m.rt.Bytecode(), m.rt.BPFFS(), m.logger)
			for _, a := range plan.StoreActions {
				if err := txExec.Execute(ctx, a); err != nil {
					m.logger.WarnContext(ctx, "dry-run store action failed", "action", fmt.Sprintf("%T", a), "error", err)
					continue
				}
			}
			plan.Violations, plan.LiveOrphans = m.evaluateCoherency(ctx, tx, opts)
			return errRollback
		})
		if txErr != nil && !errors.Is(txErr, errRollback) {
			return plan, fmt.Errorf("dry-run store gc: %w", txErr)
		}
	} else {
		plan.Violations, plan.LiveOrphans = evaluateCoherencyFromState(state, opts)
	}

	return plan, nil
}

// evaluateCoherency gathers state from the given store and evaluates
// GC rules, returning actionable violations and the live orphan count.
func (m *Manager) evaluateCoherency(ctx context.Context, store platform.Store, opts GCOptions) ([]coherency.Violation, int) {
	state, err := coherency.GatherState(ctx, store, m.kernel, m.Layout())
	if err != nil {
		m.logger.WarnContext(ctx, "failed to gather state for post-store GC", "error", err)
		return nil, 0
	}

	gcRules := buildGCRules(opts)
	violations := coherency.Evaluate(state, gcRules)

	var liveOrphans int
	if !opts.Prune {
		liveOrphans = state.LiveOrphans()
	}

	return violations, liveOrphans
}

// evaluateCoherencyFromState runs the coherency rules against an
// already-gathered ObservedState, avoiding a redundant GatherState
// call. Used when no store actions were computed and the existing
// state is still valid. Returns all violations including
// diagnostic-only (no-intent) findings; callers that execute repairs
// must filter to Intent != nil themselves.
func evaluateCoherencyFromState(state *coherency.ObservedState, opts GCOptions) ([]coherency.Violation, int) {
	gcRules := buildGCRules(opts)
	violations := coherency.Evaluate(state, gcRules)

	var liveOrphans int
	if !opts.Prune {
		liveOrphans = state.LiveOrphans()
	}

	return violations, liveOrphans
}

// buildGCRules constructs the rule set from options. The destructive
// prune-live-orphans rule is opt-in and only included when
// opts.Prune is set.
func buildGCRules(opts GCOptions) []coherency.Rule {
	gcRules := coherency.Rules()
	if opts.Prune {
		gcRules = append(gcRules, coherency.PruneRule())
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
	return gcRules
}

// ExecuteGC executes a previously computed GCPlan and returns
// statistics about what was removed. Coherency violations are
// re-gathered after store actions execute so that the coherency
// rules see the post-deletion state (e.g. orphan filesystem
// artefacts left behind by deleted DB rows).
func (m *Manager) ExecuteGC(ctx context.Context, writeLock lock.WriterScope, plan GCPlan, opts GCOptions) (result GCResult, retErr error) {
	start := time.Now()

	// Phase 1: execute store-level actions within a single
	// transaction.
	if len(plan.StoreActions) > 0 {
		var executed []action.Action
		if err := m.store.RunInTransaction(ctx, func(tx platform.Store) error {
			txExec := newExecutor(tx, m.kernel, m.rt.Bytecode(), m.rt.BPFFS(), m.logger)
			for _, a := range plan.StoreActions {
				if err := txExec.Execute(ctx, a); err != nil {
					m.logger.WarnContext(ctx, "store gc action failed", "action", fmt.Sprintf("%T", a), "error", err)
					continue
				}
				executed = append(executed, a)
			}
			return nil
		}); err != nil {
			return result, fmt.Errorf("store gc: %w", err)
		}
		result.ProgramsRemoved = countByType[action.DeleteProgram](executed)
		result.LinksRemoved = countByType[action.DeleteLink](executed)
		result.DispatchersRemoved = countByType[action.DeleteDispatcher](executed)
	}

	// Phase 2: coherency violations. When store actions were
	// executed, re-gather state to see the post-deletion observation
	// (artefacts may have become orphaned by store deletions).
	// When no store actions exist, the plan's violations from
	// ComputeGC are already current; reuse them.
	var violations []coherency.Violation
	if len(plan.StoreActions) > 0 {
		var liveOrphans int
		violations, liveOrphans = m.evaluateCoherency(ctx, m.store, opts)
		result.LiveOrphans = liveOrphans
	} else {
		violations = plan.Violations
		result.LiveOrphans = plan.LiveOrphans
	}

	for _, v := range violations {
		if v.Intent == nil {
			continue
		}
		desc := v.Intent.Describe()
		if err := m.executor.ExecuteAll(ctx, v.Intent.Actions()); err != nil {
			m.logger.WarnContext(ctx, "gc operation failed", "op", desc, "error", err)
			retErr = fmt.Errorf("gc operation failed: %s: %w", desc, err)
			continue
		}
		m.logger.InfoContext(ctx, "gc operation applied", "op", desc)
		switch v.Category {
		case "gc-dispatcher":
			result.DispatchersRemoved++
		case "gc-orphan-pin":
			result.OrphanPinsRemoved++
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

	return result, retErr
}

// GCWithOptions runs garbage collection with the given options.
// Equivalent to GCRemediate -- the original entry point is preserved
// for callers (audit, e2e harness) that already hold the lock and
// want a single all-in-one call.
func (m *Manager) GCWithOptions(ctx context.Context, writeLock lock.WriterScope, opts GCOptions) (GCResult, error) {
	return m.GCRemediate(ctx, writeLock, opts)
}

// GCRemediate is the under-lock half of the scan / remediate split.
// It scans state from scratch (the authoritative read), executes any
// remediation actions, and returns the result. An empty scan
// short-circuits with the zero GCResult and no lock-time work other
// than the scan itself; callers that already ran a lockless GCScan
// pay one extra scan in exchange for not racing against state
// changes between the lockless scan and the lock acquisition.
//
// The lockless GCScan result is intentionally not passed in. It
// is a hint only: gcOnEntry uses it to decide whether the lock is
// worth acquiring; the authoritative set comes from the under-lock
// re-scan here.
func (m *Manager) GCRemediate(ctx context.Context, writeLock lock.WriterScope, opts GCOptions) (GCResult, error) {
	plan, err := m.GCScan(ctx, opts)
	if err != nil {
		return GCResult{}, err
	}
	if plan.IsEmpty() {
		return GCResult{}, nil
	}
	return m.ExecuteGC(ctx, writeLock, plan, opts)
}

// gcOnEntry prepares a mutating manager operation by running GC on
// the way in. Every mutating method (Load, Attach, Detach, Unload)
// goes through here, so every mutation begins with a coherency
// sweep that reconciles store, kernel, and bpffs state.
//
// Re-entry: if the context already carries the opActiveKey marker
// (a nested mutating call within the same outer operation), GC is
// skipped because the outer caller already swept. The marker is
// set before the GC call so any internal recursion sees it and
// bails.
//
// computeStoreGC has its own gate on kernel-enumeration
// completeness inside ComputeGC; partial enumeration skips the
// store-deletion phases rather than misclassifying healthy state.
//
// There is no flag, no endOp, and no constructor default. Every
// process's first mutation runs GC because every mutation runs
// GC. The bpfman-server does not need a separate bootstrap GC
// call.
func (m *Manager) gcOnEntry(ctx context.Context, writeLock lock.WriterScope) (context.Context, error) {
	if ctx.Value(opActiveKey{}) != nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, opActiveKey{}, true)
	if _, err := m.GC(ctx, writeLock); err != nil {
		return ctx, err
	}
	return ctx, nil
}
