// Package manager provides high-level orchestration for BPF program
// lifecycle management using the fetch/compute/execute pattern.
//
// # Overview
//
// The manager is the primary API for loading, attaching, detaching, and
// querying BPF programs. It coordinates between three layers:
//
//   - Store: SQLite database for program and link metadata
//   - Kernel: cilium/ebpf adapter for BPF syscalls
//   - Filesystem: bpffs pins and bytecode persistence
//
// Callers include the gRPC server (server/) for daemon mode and the CLI
// (cmd/bpfman/) for direct invocation. Both delegate to Manager methods
// after acquiring the appropriate locks.
//
// # Fetch/Compute/Execute
//
// Mutating operations generally follow a phased pattern:
//
//  1. FETCH: gather state from store, kernel, and filesystem
//  2. COMPUTE: determine actions as reified effect values (action/)
//  3. EXECUTE: interpret actions via ActionExecutor
//
// Variations exist: Unload() is a straight FETCH -> COMPUTE -> EXECUTE
// pipeline; AttachXDP() and AttachTC() interleave kernel I/O between fetch
// and compute because dispatcher creation/reuse must be observed before
// slot selection can be computed; AttachTCX() computes ordering before
// kernel attach because it depends on existing link state; Detach() queries
// after execution to determine dispatcher cleanup needs.
// These deviations are driven by kernel observability: some decisions can
// only be made after observing post-operation kernel or netlink state.
//
// Load() is intentionally staged rather than action-driven: it
// must sequence kernel load, filesystem publish, and store transactions
// to preserve atomicity and rollback semantics. Read-only methods (Get,
// ListPrograms, GetLink, etc.) are fetch-only and do not build action
// lists, since they are purely observational.
//
// The theme is separating state gathering from effect execution even
// when phases interleave.
//
// The platform layer (platform/) provides the I/O abstractions for
// BPF operations. Minor exceptions exist (e.g., GetHostInfo calls
// unix.Uname directly).
//
// # Atomic Load Model
//
// Load operations provide atomic semantics: either a program is fully
// loaded with metadata persisted, or nothing is left behind.
//
//  1. Load program into kernel and pin to bpffs
//  2. On success: persist metadata to store in a single transaction
//  3. On failure: cleanup kernel state; nothing written to store
//  4. GC handles orphans from crashes
//
// This avoids intermediate states like "loading" or "error" in the
// database. Programs only appear in the store after successful load.
//
// # Rollback and Error Reporting
//
// Failed mutating operations (Load, Unload, Attach*, Detach*)
// return *ManagerError, which wraps the underlying error and includes a
// structured OperationOutcome containing:
//
//   - Timeline of completed and failed steps
//   - Rollback errors (if cleanup also failed)
//   - Residual artefacts that could not be cleaned
//
// Callers can use errors.As to extract structured diagnostics.
// Read-only methods (Get, ListPrograms, ListLinks, ListLinksByProgram,
// GetLink, GetLinkInfo, FindLoadedProgramByMetadata) return plain errors.
//
// # Garbage Collection
//
// GC reconciles the store against kernel and filesystem state. It runs:
//
//   - At startup before accepting requests
//   - Before mutating operations (via GCIfNeeded)
//   - On read operations if mutations occurred since the last GC
//
// GC removes:
//
//   - Store entries for programs no longer in the kernel
//   - Store entries where bpffs pins are missing (ID reuse detection)
//   - Orphan dispatcher directories and pins
//   - Stale staging directories from interrupted operations
//
// The coherency engine (coherency.go) evaluates rules to detect
// violations and produces executable remediation operations.
//
// # Attachment Types
//
// The manager supports multiple BPF attachment points:
//
//   - XDP: network interface ingress via dispatcher programs
//   - TC: traffic control ingress/egress via dispatchers
//   - TCX: traffic control ingress/egress using native kernel multi-prog (no dispatchers)
//   - Tracepoint: kernel tracepoints (sched/sched_switch, etc.)
//   - Kprobe/Kretprobe: kernel function entry/return
//   - Uprobe/Uretprobe: userspace function entry/return
//   - Fentry/Fexit: fast kernel function tracing
//
// XDP and TC attachments use dispatcher programs that chain multiple
// extension programs at a single attach point. The dispatcher state
// is tracked in the store and reconciled by GC.
//
// # Concurrency
//
// The manager itself is not safe for concurrent use. Callers must
// serialise access, typically via the lock package (lock/) which
// provides writer-exclusive locking at the server level.
//
// GC has its own mutex (gcMu) to coordinate with the mutation flag,
// allowing the server to determine when GC should run.
//
// # Dependencies
//
// Create a Manager via New(), providing:
//
//   - FilesystemContext: capability token proving bpffs is mounted
//   - Store: database interface (platform.Store)
//   - KernelOperations: BPF syscall adapter
//   - ProgramDiscoverer: kernel program enumeration
//   - ImagePuller: optional OCI image puller for container images
//   - Logger: structured logger with op_id support
//
// The FilesystemContext is obtained from bpfmanfs/runtime.New() after
// ensuring directories exist and bpffs is mounted.
package manager
