// Package outcome models the externally visible result of
// manager-level operations.
//
// # Overview
//
// An [OperationOutcome] is a structured record of what happened during
// a multi-step operation (load, unload, attach, detach, GC). It is
// designed for operators reading failure reports, automation consuming
// JSON output, and reconciliation logic determining what residue
// remains after a failed operation.
//
// # Timeline-First Design
//
// All steps -- both primary and rollback -- appear in a single
// ordered [OperationOutcome.Timeline] array. Each [TimelineEntry]
// records its phase (primary or rollback), status (completed, failed,
// or skipped), kind, target, and optional details. This makes it
// straightforward to reconstruct what happened in chronological order.
//
// # Step Taxonomy
//
// [StepKind] values form a stable taxonomy of externally meaningful
// operations. A single StepKind may correspond to multiple internal
// calls, or a single call may produce multiple steps. Consumers must
// ignore unknown StepKind values.
//
// Step kinds are grouped by layer:
//
//   - manager.*: pre-kernel operations (preflight checks)
//   - image.*: OCI image pull and program discovery
//   - kernel.*: BPF syscalls, attach/detach, pin operations
//   - store.*: database CRUD and garbage collection
//   - gc.*: rule-engine orphan removal
//   - fs.*: bytecode publish and program removal
//
// # Residual State
//
// On failure, the outcome probes actual kernel and filesystem state
// to populate [OperationOutcome.Residual] with [Artefact] values
// describing what still exists. This implements a "never lie"
// principle: residual state is observed, not inferred from step
// history. The [OperationOutcome.SystemState] field is computed from
// the residual: "clean" (no leftover), "inconsistent" (residue
// exists), or "unknown" (observation failed).
//
// When the system state is "inconsistent",
// [ComputeManualCleanupCommands] generates idempotent cleanup
// commands as argv slices.
//
// # Recorder
//
// [ManagerOperationRecorder] appends steps to an OperationOutcome
// while enforcing invariants. It tracks the current phase, prevents
// recording primary steps after failure, and computes derived fields
// via [ManagerOperationRecorder.Finalise]. The recorder is
// intentionally minimal: it does not know about manager semantics,
// rollback policy, or step ordering.
//
// # Detail Types
//
// Operation-specific detail types ([ProgramDetails], [LinkDetails],
// [DispatcherDetails], [GCPhaseDetails], [OrphanDetails],
// [ImageDetails], [TCFilterDetails]) provide structured information
// in [TimelineEntry.Details]. All detail types are JSON-serialisable.
package outcome
