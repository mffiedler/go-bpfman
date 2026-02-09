package outcome

import (
	"fmt"
	"strconv"
	"time"
)

// Status indicates overall operation result.
type Status string

const (
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
)

// Phase indicates which phase of the operation a step belongs to.
type Phase string

const (
	PhasePrimary  Phase = "primary"
	PhaseRollback Phase = "rollback"
)

// StepStatus indicates whether a step completed, failed, or was skipped.
type StepStatus string

const (
	StepStatusCompleted StepStatus = "completed"
	StepStatusFailed    StepStatus = "failed"
	StepStatusSkipped   StepStatus = "skipped"
	StepStatusWarned    StepStatus = "warned"
)

// StepKind identifies the type of operation step.
//
// StepKind is a stable taxonomy of externally meaningful steps, not a direct
// reflection of function names. A single StepKind may correspond to multiple
// internal calls (e.g., "image.discover" involves reads, parses, validation),
// or a single call may produce multiple steps (e.g., batch operations).
//
// Consumers MUST ignore unknown StepKind values rather than failing.
// New values may be added in future versions.
type StepKind string

const (
	// Pre-kernel operations (manager layer)
	StepKindPreflight        StepKind = "manager.preflight"
	StepKindPullImage        StepKind = "image.pull"
	StepKindDiscoverPrograms StepKind = "image.discover"

	// Kernel operations (maps to platform.KernelOperations)
	StepKindKernelLoad       StepKind = "kernel.load_program"
	StepKindKernelUnload     StepKind = "kernel.unload_program"
	StepKindKernelDetachLink StepKind = "kernel.detach_link"
	StepKindKernelRemovePin  StepKind = "kernel.remove_pin"

	// Store operations (maps to platform.Store)
	StepKindStoreSaveProgram      StepKind = "store.save_program"
	StepKindStoreDeleteProgram    StepKind = "store.delete_program"
	StepKindStoreSaveLink         StepKind = "store.save_link"
	StepKindStoreDeleteLink       StepKind = "store.delete_link"
	StepKindStoreSaveDispatcher   StepKind = "store.save_dispatcher"
	StepKindStoreDeleteDispatcher StepKind = "store.delete_dispatcher"

	// Dispatcher operations (XDP/TC attach via dispatcher)
	StepKindAttachXDPDispatcher StepKind = "kernel.attach_xdp_dispatcher"
	StepKindAttachTCDispatcher  StepKind = "kernel.attach_tc_dispatcher"
	StepKindAttachExtension     StepKind = "kernel.attach_extension"
	StepKindDetachTCFilter      StepKind = "kernel.detach_tc_filter"

	// Direct attach operations (TCX, tracing)
	StepKindAttachTCX        StepKind = "kernel.attach_tcx"
	StepKindAttachTracepoint StepKind = "kernel.attach_tracepoint"
	StepKindAttachKprobe     StepKind = "kernel.attach_kprobe"
	StepKindAttachUprobe     StepKind = "kernel.attach_uprobe"
	StepKindAttachFentry     StepKind = "kernel.attach_fentry"
	StepKindAttachFexit      StepKind = "kernel.attach_fexit"

	// GC operations - Phase 1: Store GC
	StepKindStoreGCPrograms    StepKind = "store.gc_programs"
	StepKindStoreGCLinks       StepKind = "store.gc_links"
	StepKindStoreGCDispatchers StepKind = "store.gc_dispatchers"

	// GC operations - Phase 2: Rule Engine
	StepKindGCRemoveOrphan StepKind = "gc.remove_orphan"

	// Filesystem operations
	StepKindFSPublish       StepKind = "fs.publish_bytecode"
	StepKindFSRemoveProgram StepKind = "fs.remove_program"
)

// TimelineEntry represents a single step in the operation timeline.
// All steps (primary and rollback) appear in chronological order.
type TimelineEntry struct {
	// Seq is the sequence number (1-based) for ordering.
	Seq int `json:"seq"`

	// Timestamp is when this step was recorded (RFC3339 with nanoseconds).
	// Useful for correlating with system logs, k8s logs, and application logs.
	Timestamp time.Time `json:"timestamp"`

	// Phase indicates whether this is a primary or rollback step.
	Phase Phase `json:"phase"`

	// Status indicates whether the step completed, failed, or was skipped.
	Status StepStatus `json:"status"`

	// Kind identifies what type of operation this step represents.
	Kind StepKind `json:"kind"`

	// Target identifies what the step operated on.
	// Format depends on Kind (see Target Conventions in plan).
	Target string `json:"target,omitempty"`

	// Error is the error message if this step failed.
	Error string `json:"error,omitempty"`

	// Details contains operation-specific information.
	// Must be JSON-serialisable.
	Details any `json:"details,omitempty"`
}

// OperationOutcome represents the result of any multi-step operation.
//
// The timeline-first design puts all steps in a single ordered array,
// making it easy to understand the sequence of events.
type OperationOutcome struct {
	// OpID is the correlation ID for log/trace correlation.
	OpID uint64 `json:"op_id,omitempty"`

	// Status indicates overall success or failure.
	Status Status `json:"status"`

	// PrimaryError is the error from the primary operation (if failed).
	PrimaryError string `json:"primary_error,omitempty"`

	// RollbackErrors contains structured errors from the rollback phase.
	// Each entry pairs a step index with its error message.
	// Only populated when rollback was attempted and failed.
	RollbackErrors []RollbackError `json:"rollback_errors,omitempty"`

	// Timeline contains all steps in chronological order.
	// Each entry has a phase (primary/rollback) and status (completed/failed/skipped).
	Timeline []TimelineEntry `json:"timeline,omitempty"`

	// Residual is the set of artefacts that still exist after the operation
	// (including any rollback attempts). Populated by probing actual state.
	//
	// "Never lie" means Residual is populated by probing actual kernel/FS
	// state, not inferred from step history. Residual MUST contain only
	// unexpected leftovers, not "things we successfully created".
	//
	// Use `bpfman doctor` / `bpfman gc` for full system inspection.
	Residual []Artefact `json:"residual,omitempty"`

	// ResidualError is set when state probing fails.
	// This prevents false "clean" reports when observation couldn't complete.
	ResidualError string `json:"residual_error,omitempty"`

	// SystemState is "clean", "inconsistent", or "unknown".
	// Computed from Residual/ResidualError and stored for JSON output.
	SystemState string `json:"system_state"`

	// ManualCleanupRequired indicates operator intervention is required.
	// True for "inconsistent" or "unknown" states on failure.
	ManualCleanupRequired bool `json:"manual_cleanup_required"`

	// ManualCleanupCommands contains cleanup commands as argv slices.
	// Each entry is a command ready for exec (e.g., ["bpfman", "unload", "123"]).
	// Only populated when ManualCleanupRequired is true and state is "inconsistent".
	ManualCleanupCommands [][]string `json:"manual_cleanup_commands,omitempty"`
}

// StepHandle is an opaque reference to a recorded timeline entry.
// The plan interpreter uses handles to attach details after the fact
// via SetDetails, avoiding the need to search by (kind, target).
type StepHandle struct{ ix int }

// InvalidStepHandle returns a handle that refers to no entry.
func InvalidStepHandle() StepHandle { return StepHandle{ix: -1} }

// Valid reports whether h refers to an actual timeline entry.
func (h StepHandle) Valid() bool { return h.ix >= 0 }

// NewStep constructs a Step with the given kind, target, and optional details.
func NewStep(kind StepKind, target string, details any) Step {
	return Step{Kind: kind, Target: target, Details: details}
}

// Step is the internal representation used during recording.
// It gets converted to TimelineEntry when added to the timeline.
type Step struct {
	// Kind identifies what type of operation this step represents.
	Kind StepKind `json:"kind"`

	// Target identifies what the step operated on.
	Target string `json:"target,omitempty"`

	// Details contains operation-specific information.
	Details any `json:"details,omitempty"`

	// Error is the error message if this step failed.
	Error string `json:"error,omitempty"`

	// Timestamp is when the operation was attempted (not when it was recorded).
	// If zero, the recorder uses time.Now() when the step is recorded.
	// Callers should set this before starting the operation for accurate timing.
	Timestamp time.Time `json:"-"`
}

// ComputeSystemState returns "clean", "inconsistent", or "unknown" based on residual state.
//
// This function examines the residual artefacts (populated by probing actual
// kernel/filesystem state at operation end) rather than inferring from step
// history. This implements the "never lie" principle.
//
// Definition:
//   - "clean": No unexpected residue remains.
//   - "inconsistent": Residue exists that requires manual cleanup.
//   - "unknown": Observation failed; we cannot determine actual state.
//
// IMPORTANT: Success implies clean BY CONTRACT, not by verification.
// On success we skip probing (for performance) and report "clean" by definition.
func ComputeSystemState(status Status, residual []Artefact, residualError string) string {
	// Success fast-path: clean by contract, no verification
	if status == StatusSuccess {
		return "clean"
	}
	if residualError != "" {
		return "unknown"
	}
	if len(residual) == 0 {
		return "clean"
	}
	return "inconsistent"
}

// ComputeManualCleanupRequired returns true if operator intervention is required.
// Returns true for BOTH "inconsistent" (known residue) AND "unknown"
// (observation failed) - in unknown case, operator must verify manually.
func ComputeManualCleanupRequired(status Status, systemState string) bool {
	if status != StatusFailure {
		return false
	}
	return systemState == "inconsistent" || systemState == "unknown"
}

// ComputeManualCleanupCommands returns cleanup commands as argv slices.
//
// Constraints:
//   - ONLY returns commands when systemState == "inconsistent"
//   - Returns empty if "clean" (no residue)
//   - Returns empty if "unknown" (incomplete observation undermines "never lie")
//   - Generated from residual artefacts (actual state), not step history
//   - Commands are idempotent (safe to run multiple times)
//   - Commands use absolute identifiers (kernel IDs, not names)
//   - Each command is an argv slice ready for exec (no shell parsing needed)
//   - DEDUPLICATE: Same kernel ID or pin path yields only one command
//   - SUPPRESSION: program-level commands suppress lower-level commands
//     for maps_dir / pins referring to the same logical program
func ComputeManualCleanupCommands(systemState string, residual []Artefact) [][]string {
	if systemState != "inconsistent" {
		return nil
	}

	var cmds [][]string
	seenKernelIDs := make(map[uint32]bool)
	seenLinkIDs := make(map[uint32]bool)
	needsGC := false

	// First pass: collect program_pin with kernel_id (highest priority)
	for _, a := range residual {
		if a.Kind == ArtefactProgramPin && a.KernelID != 0 {
			if !seenKernelIDs[a.KernelID] {
				seenKernelIDs[a.KernelID] = true
				cmds = append(cmds, []string{"bpfman", "unload", strconv.FormatUint(uint64(a.KernelID), 10)})
			}
		}
	}

	// Second pass: collect link_pin with link_id
	for _, a := range residual {
		if a.Kind == ArtefactLinkPin && a.LinkID != 0 {
			if !seenLinkIDs[a.LinkID] {
				seenLinkIDs[a.LinkID] = true
				cmds = append(cmds, []string{"bpfman", "detach", "--id", strconv.FormatUint(uint64(a.LinkID), 10)})
			}
		}
	}

	// Third pass: check for residue that needs GC
	// (maps_dir without associated kernel_id, orphan pins)
	for _, a := range residual {
		switch a.Kind {
		case ArtefactMapsDir:
			// If we already have an unload command for this kernel_id, skip
			if a.KernelID != 0 && seenKernelIDs[a.KernelID] {
				continue
			}
			needsGC = true
		case ArtefactProgramPin:
			// If no kernel_id, we can't unload by ID
			if a.KernelID == 0 {
				needsGC = true
			}
		case ArtefactLinkPin:
			// If no link_id, we can't detach by ID
			if a.LinkID == 0 {
				needsGC = true
			}
		case ArtefactDispatcher, ArtefactTCFilter:
			// These require GC or specialized cleanup
			needsGC = true
		}
	}

	if needsGC {
		cmds = append(cmds, []string{"bpfman", "gc"})
	}

	return cmds
}

// RollbackError pairs a rollback step failure with its position.
type RollbackError struct {
	Step int    `json:"step"`
	Err  string `json:"error"`
}

// ArtefactKind identifies the type of residue artefact.
// Consumers MUST ignore unknown values rather than failing.
type ArtefactKind string

const (
	ArtefactProgramPin ArtefactKind = "program_pin"
	ArtefactMapsDir    ArtefactKind = "maps_dir"
	ArtefactLinkPin    ArtefactKind = "link_pin"
	ArtefactDispatcher ArtefactKind = "dispatcher"
	ArtefactTCFilter   ArtefactKind = "tc_filter"
	ArtefactProgramDir ArtefactKind = "program_dir"
)

// Artefact represents a kernel/filesystem object that exists at operation end.
// Used by "never lie" principle - we probe actual state, not inferred state.
type Artefact struct {
	Kind     ArtefactKind `json:"kind"`
	KernelID uint32       `json:"kernel_id,omitempty"`
	LinkID   uint32       `json:"link_id,omitempty"`
	Path     string       `json:"path,omitempty"`
}

// String returns a human-readable representation of the artefact.
func (a Artefact) String() string {
	switch a.Kind {
	case ArtefactProgramPin:
		if a.KernelID != 0 {
			return fmt.Sprintf("program_pin(kernel_id=%d, path=%s)", a.KernelID, a.Path)
		}
		return fmt.Sprintf("program_pin(path=%s)", a.Path)
	case ArtefactMapsDir:
		return fmt.Sprintf("maps_dir(path=%s)", a.Path)
	case ArtefactLinkPin:
		if a.LinkID != 0 {
			return fmt.Sprintf("link_pin(link_id=%d, path=%s)", a.LinkID, a.Path)
		}
		return fmt.Sprintf("link_pin(path=%s)", a.Path)
	case ArtefactDispatcher:
		return fmt.Sprintf("dispatcher(kernel_id=%d)", a.KernelID)
	case ArtefactTCFilter:
		return fmt.Sprintf("tc_filter(path=%s)", a.Path)
	case ArtefactProgramDir:
		return fmt.Sprintf("program_dir(kernel_id=%d, path=%s)", a.KernelID, a.Path)
	default:
		return fmt.Sprintf("%s(kernel_id=%d, link_id=%d, path=%s)", a.Kind, a.KernelID, a.LinkID, a.Path)
	}
}
