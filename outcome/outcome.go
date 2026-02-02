// Package outcome models the externally visible result of manager-level operations.
//
// It is a first-class description of what happened, intended for:
//   - operators (human-readable failure reports)
//   - automation (JSON output for CI/K8s)
//   - post-mortems (what succeeded, what failed, what cleanup was attempted)
//   - reconciliation logic (what residue remains)
//
// outcome is intentionally dumber than the manager. It does not know:
//   - rollback policy
//   - step ordering semantics
//   - GC rules
//   - attach vs load differences
//
// It only defines structure, invariants, and recording discipline.
package outcome

import (
	"fmt"
	"strconv"
)

// Status indicates overall operation result.
type Status string

const (
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
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

	// Kernel operations (maps to interpreter.KernelOperations)
	StepKindKernelLoad       StepKind = "kernel.load_program"
	StepKindKernelUnload     StepKind = "kernel.unload_program"
	StepKindKernelDetachLink StepKind = "kernel.detach_link"
	StepKindKernelRemovePin  StepKind = "kernel.remove_pin"

	// Store operations (maps to interpreter.Store)
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
)

// ManagerOperationOutcome represents the result of any multi-step operation.
//
// INVARIANT: Cleanup never mutates Completed/Failed/Skipped. Once a step
// is recorded in Completed, it remains there even if cleanup succeeds.
// The Cleanup field separately tracks what cleanup operations were attempted.
type ManagerOperationOutcome struct {
	// OpID is the correlation ID for log/trace correlation.
	OpID uint64 `json:"op_id,omitempty"`

	// Status indicates success or failure.
	Status Status `json:"status"`

	// Error is the public-facing error message (JSON-safe string).
	// Set at the manager boundary to match the returned error.
	Error string `json:"error,omitempty"`

	// Completed steps in the primary operation (immutable after recording).
	Completed []Step `json:"completed,omitempty"`

	// Failed is the step that caused the operation to fail.
	Failed *Step `json:"failed,omitempty"`

	// Skipped steps that were not attempted due to earlier failure.
	Skipped []Step `json:"skipped,omitempty"`

	// Cleanup tracks rollback/cleanup operations (only populated if needed).
	Cleanup *CleanupOutcome `json:"cleanup,omitempty"`

	// Observed is the set of RESIDUE artefacts relevant to THIS operation
	// that still exist at the end of the operation (after any cleanup was
	// attempted). This is not a full system snapshot.
	//
	// "Never lie" means Observed is populated by probing actual kernel/FS
	// state, not inferred from step history. Observed MUST contain only
	// unexpected leftovers (residue), not "things we successfully created".
	//
	// Use `bpfman doctor` / `bpfman gc` for full system inspection.
	Observed []Artefact `json:"observed,omitempty"`

	// ObservedError is set when state probing fails.
	// This prevents false "clean" reports when observation couldn't complete.
	ObservedError string `json:"observed_error,omitempty"`
}

// SystemState returns "clean", "inconsistent", or "unknown" based on OBSERVED state.
//
// This method examines the Observed artefacts (populated by probing actual
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
// Any latent inconsistency from a successful operation is detected by later
// `bpfman doctor` or `bpfman gc` runs.
func (o ManagerOperationOutcome) SystemState() string {
	// Success fast-path: clean by contract, no verification
	if o.Status == StatusSuccess {
		return "clean"
	}
	if o.ObservedError != "" {
		return "unknown"
	}
	if len(o.Observed) == 0 {
		return "clean"
	}
	return "inconsistent"
}

// Started returns true if at least one real step was attempted.
// Computed (not stored) to avoid drift.
func (o ManagerOperationOutcome) Started() bool {
	return len(o.Completed) > 0 || o.Failed != nil || len(o.Skipped) > 0
}

// NeedsManualCleanup returns true if operator intervention is required.
// Returns true for BOTH "inconsistent" (known residue) AND "unknown"
// (observation failed) - in unknown case, operator must verify manually.
func (o ManagerOperationOutcome) NeedsManualCleanup() bool {
	if o.Status != StatusFailure {
		return false
	}
	// "unknown" means "assume manual verification/cleanup is required"
	if o.ObservedError != "" {
		return true
	}
	return len(o.Observed) > 0
}

// ManualCleanupCommands returns cleanup commands as argv slices.
//
// Constraints:
//   - ONLY returns commands when SystemState() == "inconsistent"
//   - Returns empty if "clean" (no residue)
//   - Returns empty if "unknown" (incomplete observation undermines "never lie")
//   - Generated from Observed artefacts (actual state), not step history
//   - Commands are idempotent (safe to run multiple times)
//   - Commands use absolute identifiers (kernel IDs, not names)
//   - DEDUPLICATE: Same kernel ID or pin path yields only one command
//   - SUPPRESSION: program-level commands suppress lower-level commands
//     for maps_dir / pins referring to the same logical program
func (o ManagerOperationOutcome) ManualCleanupCommands() [][]string {
	if o.SystemState() != "inconsistent" {
		return nil
	}

	var cmds [][]string
	seenKernelIDs := make(map[uint32]bool)
	seenLinkIDs := make(map[uint32]bool)
	needsGC := false

	// First pass: collect program_pin with kernel_id (highest priority)
	for _, a := range o.Observed {
		if a.Kind == ArtefactProgramPin && a.KernelID != 0 {
			if !seenKernelIDs[a.KernelID] {
				seenKernelIDs[a.KernelID] = true
				cmds = append(cmds, []string{"bpfman", "unload", strconv.FormatUint(uint64(a.KernelID), 10)})
			}
		}
	}

	// Second pass: collect link_pin with link_id
	for _, a := range o.Observed {
		if a.Kind == ArtefactLinkPin && a.LinkID != 0 {
			if !seenLinkIDs[a.LinkID] {
				seenLinkIDs[a.LinkID] = true
				cmds = append(cmds, []string{"bpfman", "detach", "--id", strconv.FormatUint(uint64(a.LinkID), 10)})
			}
		}
	}

	// Third pass: check for residue that needs GC
	// (maps_dir without associated kernel_id, orphan pins)
	for _, a := range o.Observed {
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

// Step represents a single operation step.
type Step struct {
	// Kind identifies what type of operation this step represents.
	Kind StepKind `json:"kind"`

	// Target identifies what the step operated on.
	// Format depends on Kind (see Target Conventions in plan).
	Target string `json:"target,omitempty"`

	// Details contains operation-specific information.
	// Must be JSON-serialisable.
	Details any `json:"details,omitempty"`

	// Error is the error message if this step failed.
	Error string `json:"error,omitempty"`
}

// CleanupOutcome tracks rollback/cleanup results.
type CleanupOutcome struct {
	Status    Status `json:"status"`
	Completed []Step `json:"completed,omitempty"`
	Failed    []Step `json:"failed,omitempty"`
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
	default:
		return fmt.Sprintf("%s(kernel_id=%d, link_id=%d, path=%s)", a.Kind, a.KernelID, a.LinkID, a.Path)
	}
}
