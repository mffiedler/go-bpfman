// Package action defines reified effects for BPF program lifecycle
// operations. "Reified" means the effects are represented as ordinary
// data values (structs) rather than being executed immediately --
// a function call becomes a value you can inspect, store, or pass
// around before deciding to execute it.
//
// # Overview
//
// Actions are pure data structures that describe what should happen
// without performing any I/O. The manager computes a list of actions
// from observed state, then hands them to the manager's executor for
// interpretation. This separation keeps domain logic testable without
// a real kernel or database.
//
// # Design
//
// Actions are intentionally generic rather than program-type-specific.
// There is no LoadXDPProgram or LoadTCProgram; there is just
// [LoadProgram] which carries a [bpfman.LoadSpec] containing the
// program type. Similarly, [SaveLink] works with [bpfman.LinkRecord]
// which has a Kind discriminator and a LinkDetails sealed interface
// for type-specific fields.
//
// This keeps the action set small (operations like load, unload,
// save, delete) rather than exploding to N actions x M program types.
// Adding a new program type requires new constants and detail structs,
// not new action types.
//
// # Action Types
//
// Store actions:
//
//   - [SaveProgram]: persist program metadata to the store
//   - [DeleteProgram]: remove program metadata from the store
//   - [SaveLink]: persist link metadata to the store
//   - [DeleteLink]: remove link metadata from the store
//   - [SaveDispatcher]: create or update dispatcher state
//   - [DeleteDispatcher]: remove dispatcher state
//
// Kernel actions:
//
//   - [LoadProgram]: load a BPF program into the kernel
//   - [UnloadProgram]: unload a BPF program from the kernel
//   - [DetachLink]: remove a link pin from bpffs
//   - [RemovePin]: remove an arbitrary pin from bpffs
//   - [DetachTCFilter]: remove a legacy TC BPF filter via netlink
//
// Composite actions:
//
//   - [Batch]: group multiple actions for sequential execution
//   - [Sequence]: execute actions in order, stopping on first error
//
// # Sealed Interface
//
// The [Action] interface uses an unexported marker method (isAction)
// to prevent external implementations. All action types are defined
// in this package.
//
// # Interpretation
//
// The single consumer of action types is the executor in
// manager/executor.go, which provides the central type-switch.
// No other code should switch on action types.
package action
