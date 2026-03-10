// Package dispatcher provides XDP and TC dispatcher programs for
// multi-program chaining at a single network attach point.
//
// # Problem
//
// The Linux kernel allows only one XDP program per interface and one
// TC classifier per (interface, direction). Dispatchers solve this
// by interposing a single BPF program that chains calls to up to 10
// user programs through a single attachment.
//
// # Mechanism
//
// A dispatcher is a BPF program with 10 stub functions (prog0
// through prog9). User programs attach as BPF extensions
// (BPF_PROG_TYPE_EXT / freplace) that replace these stubs. The
// dispatcher calls each enabled slot in order; a per-slot
// proceed-on bitmask controls whether to continue or return after
// each call. Configuration is baked into .rodata at load time;
// changing it requires loading a new dispatcher instance.
//
// # Dispatcher types
//
//   - XDP ([DispatcherTypeXDP]): attached via a kernel BPF link,
//     atomically updatable across revisions.
//   - TC ingress ([DispatcherTypeTCIngress]) and TC egress
//     ([DispatcherTypeTCEgress]): attached via netlink tc filter.
//
// # Rebuild model
//
// Every attach or detach triggers a full rebuild: load a new
// dispatcher with updated .rodata, re-attach ALL extensions to it,
// atomically swap the interface attachment, persist everything in a
// single transaction, then remove the old revision's pins.
//
// The critical consequence is that every extension link gets a new
// kernel link ID on every rebuild. Between re-attachment and the
// store transaction commit the stored link IDs are stale. Once the
// transaction commits the store is consistent again, but the next
// rebuild will invalidate the IDs once more. Extension link IDs
// are not stable across rebuilds and must not be treated as durable
// identity.
//
// # GC invariant
//
// GC must not delete extension link records merely because their
// stored kernel link IDs are absent from the alive set. Instead, GC
// treats extension rows under a live dispatcher as live:
// if the dispatcher's current program ID is alive in the kernel,
// all its extension records are preserved regardless of stored link
// IDs. See dispatcher-gc.md for the full rationale.
//
// # Types
//
// [Key] identifies a dispatcher by (Type, Nsid, Ifindex). [State]
// records runtime state: revision, kernel program ID, link ID (XDP
// only), and TC filter priority.
//
// [XDPConfig] and [TCConfig] define .rodata structures.
// [LoadXDPDispatcher] and [LoadTCDispatcher] inject configuration
// into embedded BPF bytecode and return a CollectionSpec.
//
// The spec types ([XDPDispatcherAttachSpec], [TCDispatcherAttachSpec],
// [XDPExtensionAttachSpec], [TCExtensionAttachSpec]) are value
// objects describing attachment parameters.
//
// See dispatcher-model.md for filesystem layout, store schema,
// lifecycle timelines, and GC interaction detail.
package dispatcher
