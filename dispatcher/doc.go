// Package dispatcher provides XDP and TC dispatcher programs for
// multi-program chaining at a single network attach point.
//
// # Overview
//
// Linux allows only one XDP or TC-legacy program per interface per
// direction. The dispatcher pattern solves this by loading a single
// "dispatcher" program that internally chains calls to multiple user
// programs. User programs are attached as BPF extensions (freplace) to
// stub functions within the dispatcher.
//
// Each dispatcher has 10 slots (prog0–prog9). When a packet arrives,
// the dispatcher calls enabled slots in order. After each call, a
// proceed-on bitmask determines whether to continue to the next slot
// or return immediately. This enables flexible chaining: a firewall
// program might drop packets (terminating the chain) while a
// monitoring program passes packets through to the next slot.
//
// # Dispatcher Types
//
// Three dispatcher types exist:
//
//   - XDP (DispatcherTypeXDP): attached via BPF link to interface XDP hook
//   - TC ingress (DispatcherTypeTCIngress): attached via netlink to clsact qdisc
//   - TC egress (DispatcherTypeTCEgress): attached via netlink to clsact qdisc
//
// XDP dispatchers use kernel BPF links and are pinned to bpffs.
// TC dispatchers use legacy netlink attachment (tc filter add) and have
// no BPF link; only the program is pinned.
//
// # BPF Extension Mechanism
//
// User programs must be compiled as BPF extension type (freplace).
// When attached to a dispatcher slot, the extension replaces the stub
// function's implementation. The kernel validates that the extension's
// signature matches the stub (same context type and return type).
//
// The attachment creates a BPF link between the extension and the
// dispatcher. Pinning this link to bpffs keeps the attachment alive
// beyond the loading process's lifetime.
//
// # Configuration
//
// Dispatcher behaviour is configured at load time via the .rodata section:
//
//   - NumProgsEnabled: how many slots (starting from prog0) are active
//   - ChainCallActions: per-slot bitmask of return values that continue the chain
//   - RunPrios: per-slot priority values (used by manager for ordering)
//   - (XDP only) Magic, DispatcherVersion, IsXDPFrags, ProgramFlags
//
// See dispatcher/bpf/xdp_dispatcher_v2.bpf.c (struct xdp_dispatcher_conf,
// xdp_dispatcher()) and dispatcher/bpf/tc_dispatcher.bpf.c (struct
// tc_dispatcher_config, tc_dispatcher()) for the exact struct layouts
// and default return paths.
//
// The configuration is baked into the program at load time. Changing
// configuration requires loading a new dispatcher instance. The
// manager handles atomic replacement by loading a new dispatcher,
// migrating extensions, then removing the old one.
//
// # Proceed-On Semantics
//
// For XDP, the bitmask directly encodes XDP return values:
//
//	bit 0 = XDP_ABORTED
//	bit 1 = XDP_DROP
//	bit 2 = XDP_PASS
//	bit 3 = XDP_TX
//	bit 4 = XDP_REDIRECT
//
// If a program returns a value whose bit is set in ChainCallActions,
// the dispatcher proceeds to the next slot. Otherwise it returns
// immediately with that value.
//
// For TC, the bitmask is shifted by one (ret+1) to handle negative
// TC_ACT values:
//
//	bit 0 = TC_ACT_UNSPEC (-1)
//	bit 1 = TC_ACT_OK (0)
//	bit 2 = TC_ACT_RECLASSIFY (1)
//	...
//
// # Default Return
//
// If all enabled slots run and none terminate the chain early, the
// dispatcher returns the kernel default: XDP_PASS for XDP and TC_ACT_OK
// for TC.
//
// See dispatcher/bpf/xdp_dispatcher_v2.bpf.c (xdp_dispatcher()) and
// dispatcher/bpf/tc_dispatcher.bpf.c (tc_dispatcher()).
//
// # State Tracking
//
// The [Key] type uniquely identifies a dispatcher by (Type, Nsid, Ifindex).
// The [State] type records runtime state:
//
//   - Revision: incremented on each atomic update
//   - KernelID: the dispatcher program's kernel ID
//   - LinkID: the BPF link ID (XDP only; zero for TC)
//   - Priority: TC filter priority (TC only; zero for XDP)
//
// The manager stores dispatcher state in SQLite and reconciles it
// during garbage collection.
//
// # Specs
//
// The spec types ([XDPDispatcherAttachSpec], [TCDispatcherAttachSpec],
// [XDPExtensionAttachSpec], [TCExtensionAttachSpec]) are value objects
// that describe attachment parameters. They include validation methods
// and are used as inputs to manager operations, not as persistent state.
//
// # Embedded Bytecode
//
// Compiled dispatcher BPF programs are embedded via go:embed:
//
//   - xdp_dispatcher_v2.bpf.o: XDP dispatcher (derived from xdp-tools)
//   - tc_dispatcher.bpf.o: TC dispatcher
//
// The Go package loads these via [LoadXDPDispatcher] and [LoadTCDispatcher],
// which inject configuration into the spec's .rodata before the caller
// loads it into the kernel.
//
// # Compatibility Slot
//
// The embedded programs include a compat_test() function kept as an
// freplace target for libxdp compatibility checks. It is not part of
// the normal 10-slot chain.
//
// See dispatcher/bpf/xdp_dispatcher_v2.bpf.c (compat_test()) and
// dispatcher/bpf/tc_dispatcher.bpf.c (compat_test()).
//
// # Usage
//
// Typical flow for attaching an XDP program:
//
//  1. Call [LoadXDPDispatcher] with desired configuration
//  2. Load the returned spec into the kernel via cilium/ebpf
//  3. Attach the dispatcher to an interface via link.AttachXDP
//  4. Pin the dispatcher program and link to bpffs
//  5. Load user program as extension type
//  6. Call [AttachExtension] to attach user program to a slot
//  7. Pin the extension link to bpffs
//
// The manager (manager/) orchestrates these steps and handles the
// complexity of slot allocation, atomic updates, and cleanup.
package dispatcher
