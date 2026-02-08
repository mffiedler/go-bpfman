// Package interpreter defines the I/O boundary interfaces and the
// action executor for bpfman.
//
// # Overview
//
// All side effects in bpfman flow through interfaces defined here.
// Domain logic (manager/, action/, kernel/) never performs I/O
// directly; instead it depends on these abstractions, keeping
// business logic testable without a real kernel or database.
//
// The package has two concerns:
//
//  1. Interface definitions (interfaces.go): the contracts that
//     implementations in subpackages must satisfy.
//  2. Action executor (executor.go): the central type-switch that
//     interprets reified effects from the action package.
//
// # Store Interfaces
//
// The [Store] interface composes narrow, single-responsibility
// interfaces for database access:
//
//   - [ProgramReader], [ProgramWriter], [ProgramLister]: CRUD for
//     program records keyed by kernel ID
//   - [LinkReader], [LinkWriter], [LinkLister]: CRUD for link records
//     with type-specific detail tables
//   - [DispatcherStore]: XDP/TC dispatcher state management
//   - [MapOwnershipReader]: map sharing dependency queries
//   - [GarbageCollector]: bulk removal of stale entries
//   - [Transactional]: atomic multi-operation commits
//
// This composition enables narrow dependency injection: a function
// that only reads programs accepts [ProgramReader] rather than the
// full [Store].
//
// The concrete implementation lives in interpreter/store/sqlite/.
//
// # Kernel Interfaces
//
// The [KernelOperations] interface composes the kernel-side
// abstractions:
//
//   - [KernelSource]: enumerate and query kernel BPF objects
//   - [ProgramLoader]: load BPF programs and pin to bpffs
//   - [ProgramUnloader]: unpin and unload programs
//   - [ProgramAttacher]: attach programs to tracepoints, kprobes,
//     uprobes, fentry/fexit hooks
//   - [DispatcherAttacher]: attach XDP/TC dispatchers and extensions,
//     attach TCX programs
//   - [LinkDetacher]: remove link pins from bpffs
//   - [PinRemover]: remove arbitrary bpffs pins
//   - [PinInspector]: inspect pinned objects
//   - [TCFilterDetacher]: remove legacy TC filters via netlink
//   - [MapRepinner]: re-pin maps to new locations (used by CSI)
//
// The concrete implementation lives in interpreter/ebpf/, backed by
// cilium/ebpf.
//
// # Image Interfaces
//
// [ImagePuller] fetches BPF bytecode from OCI container images.
// [SignatureVerifier] checks image signatures. [ImageRef] and
// [PulledImage] describe the request and result. [ProgramDiscoverer]
// scans ELF object files for loadable BPF programs.
//
// Implementations live in interpreter/image/oci/ and
// interpreter/image/verify/.
//
// # Action Executor
//
// The [ActionExecutor] interface and its concrete implementation
// provide the central type-switch over reified effects from the
// action package. The manager computes a list of [action.Action]
// values describing what should happen, then hands them to the
// executor for interpretation.
//
// The executor dispatches each action to the appropriate [Store] or
// [KernelOperations] method. Composite actions ([action.Batch],
// [action.Sequence]) are handled recursively. This is the only place
// in the codebase that switches on action types; adding a new action
// requires exactly one new case here.
//
// [ActionExecutorWithResult] extends the base interface with
// [ActionExecutionResult], reporting how many actions completed
// before a failure. The manager uses this for structured rollback
// and error reporting.
//
// # Dependency Flow
//
// The interpreter sits between the manager and the concrete I/O
// implementations:
//
//	manager/ -> interpreter/ -> interpreter/store/sqlite/
//	                         -> interpreter/ebpf/
//	                         -> interpreter/image/oci/
//
// Pure packages (kernel/, action/) never import interpreter/.
package interpreter
