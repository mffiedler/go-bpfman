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
// active. When present, beginOp skips GC because coherence was
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

	// GC coordination - separate from request-level locking
	gcMu           sync.Mutex
	mutatedSinceGC bool

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
		mutatedSinceGC:    true,
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

// errRollback is a sentinel used to force a transaction rollback
// without indicating a real failure.
var errRollback = errors.New("rollback")

// ComputeGC gathers state and computes what GC would do, without
// executing any actions. The returned GCPlan can be inspected for
// dry-run reporting or passed to ExecuteGC for execution.
//
// State is gathered once via GatherState (which includes a full
// inspect.Snapshot). The store GC inputs are derived from the
// resulting World, avoiding the redundant kernel and store
// enumeration that previously occurred between ComputeGC's direct
// queries and the Snapshot inside evaluateCoherency.
//
// When there are store actions, they are applied inside a transaction
// that is then rolled back, so that coherency rules evaluate against
// the post-deletion state and the plan reflects the full set of
// operations that ExecuteGC would perform.
func (m *Manager) ComputeGC(ctx context.Context, writeLock lock.WriterScope, opts GCOptions) (GCPlan, error) {
	var plan GCPlan

	// Single gather pass for both store GC and coherency.
	state, err := coherency.GatherState(ctx, m.store, m.kernel, m.Layout())
	if err != nil {
		return plan, fmt.Errorf("gather state: %w", err)
	}

	// Derive store GC inputs from the correlated world.
	world := state.World()
	in := deriveStoreGCInputs(world)

	// Log pin-missing programs for visibility. A store-managed
	// program with a live kernel ID but no pin suggests the ID
	// was recycled; the program will be reaped.
	for _, p := range world.Programs {
		if p.Presence.InStore && p.Presence.InKernel && !in.kernelPrograms[p.ProgramID] {
			m.logger.InfoContext(ctx, "pin missing for live kernel ID, marking for reap",
				"program_id", p.ProgramID, "pin_path", p.PinPath())
		}
	}

	plan.StoreActions = computeStoreGC(in.programs, in.dispatchers, in.links, in.kernelPrograms, in.kernelLinks)

	// Coherency rule engine. When there are store actions, apply
	// them inside a rolled-back transaction and re-gather coherency
	// state so the rules see the post-deletion world. When there
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
	var violations []coherency.Violation
	for _, v := range coherency.Evaluate(state, gcRules) {
		if v.Op != nil {
			violations = append(violations, v)
		}
	}

	var liveOrphans int
	if !opts.Prune {
		liveOrphans = state.LiveOrphans()
	}

	return violations, liveOrphans
}

// evaluateCoherencyFromState runs the coherency rules against an
// already-gathered ObservedState, avoiding a redundant GatherState
// call. Used when no store actions were computed and the existing
// state is still valid.
func evaluateCoherencyFromState(state *coherency.ObservedState, opts GCOptions) ([]coherency.Violation, int) {
	gcRules := buildGCRules(opts)
	var violations []coherency.Violation
	for _, v := range coherency.Evaluate(state, gcRules) {
		if v.Op != nil {
			violations = append(violations, v)
		}
	}

	var liveOrphans int
	if !opts.Prune {
		liveOrphans = state.LiveOrphans()
	}

	return violations, liveOrphans
}

// buildGCRules constructs the rule set from options.
func buildGCRules(opts GCOptions) []coherency.Rule {
	gcRules := coherency.GCRules()
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
	// executed, re-gather state to see the post-deletion world
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
		if err := m.executor.ExecuteAll(ctx, v.Op.Actions); err != nil {
			m.logger.WarnContext(ctx, "gc operation failed", "op", v.Op.Description, "error", err)
			retErr = fmt.Errorf("gc operation failed: %s: %w", v.Op.Description, err)
			continue
		}
		m.logger.InfoContext(ctx, "gc operation applied", "op", v.Op.Description)
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
func (m *Manager) GCWithOptions(ctx context.Context, writeLock lock.WriterScope, opts GCOptions) (GCResult, error) {
	plan, err := m.ComputeGC(ctx, writeLock, opts)
	if err != nil {
		return GCResult{}, err
	}
	return m.ExecuteGC(ctx, writeLock, plan, opts)
}

// beginOp prepares a mutating manager operation. If the context
// already carries the opActiveKey marker (re-entry from an outer
// operation), it returns immediately. Otherwise it runs GC when
// the mutatedSinceGC flag is set, clears the flag, and returns a
// context with the marker so that nested calls skip GC.
func (m *Manager) beginOp(ctx context.Context, writeLock lock.WriterScope) (context.Context, error) {
	if ctx.Value(opActiveKey{}) != nil {
		return ctx, nil
	}

	m.gcMu.Lock()
	needsGC := m.mutatedSinceGC
	m.gcMu.Unlock()

	if needsGC {
		if _, err := m.GC(ctx, writeLock); err != nil {
			return ctx, err
		}
		m.gcMu.Lock()
		m.mutatedSinceGC = false
		m.gcMu.Unlock()
	}

	return context.WithValue(ctx, opActiveKey{}, true), nil
}

// endOp completes a mutating manager operation. If the operation
// failed, the mutatedSinceGC flag is set so that the next operation
// runs GC to clean up any partial side effects. Successful
// operations leave the flag clear because well-formed mutations do
// not produce garbage.
func (m *Manager) endOp(err error) {
	if err != nil {
		m.gcMu.Lock()
		m.mutatedSinceGC = true
		m.gcMu.Unlock()
	}
}
