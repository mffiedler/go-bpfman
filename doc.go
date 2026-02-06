// Package bpfman provides types and interfaces for BPF program management.
// This is the root package containing shared domain types used across
// the client, manager, and server components.
//
// # Program Type Overview
//
// The core types form a hierarchy reflecting the program lifecycle:
//
//	Program             - The complete domain object combining Record and Status
//	├── ProgramRecord   - DB-backed stored record (what bpfman manages)
//	│   ├── LoadSpec    - Validated load request (immutable, private fields)
//	│   ├── License/GPLCompatible - Discovered from ELF at load time
//	│   ├── Handles     - Filesystem paths (pin, maps)
//	│   └── Meta        - User-facing metadata (name, owner, labels)
//	└── ProgramStatus   - Observed runtime state (kernel + filesystem)
//	    ├── Kernel      - Live kernel program info (nil if not present)
//	    ├── Links       - Attached links with their own record/status
//	    └── Maps        - Associated kernel maps
//
// # Program Lifecycle Flow
//
// 1. LoadSpec: User provides validated input describing what to load.
// Created via NewLoadSpec() or builder methods. Immutable after construction.
//
// 2. LoadOutput: Transient result from kernel.Load(). Contains kernel-assigned
// ID, pin paths, and license discovered from ELF. Not stored - just passes
// data from I/O boundary to manager.
//
// 3. ProgramRecord: Manager combines LoadSpec + LoadOutput + user metadata and
// stores it in the database. License and GPLCompatible live here because
// they are discovered properties, not part of the original load request.
//
// 4. ProgramStatus: Observed by querying kernel and filesystem. Represents
// what actually exists now - can diverge from Record if programs are unloaded
// externally or pin files are deleted.
//
// 5. Program: Combines Record + Status. The coherency and GC systems compare
// these to detect drift and generate remediation actions.
//
// # Link Type Overview
//
// Links follow a parallel pattern to programs:
//
//	Link              - The complete domain object combining Record and Status
//	├── LinkRecord    - DB-backed stored record (what bpfman manages)
//	│   ├── ID        - Kernel-assigned or synthetic link ID
//	│   ├── ProgramID - The program this link attaches
//	│   ├── Kind      - Link type (tracepoint, kprobe, xdp, tc, etc.)
//	│   ├── PinPath   - Optional bpffs pin path
//	│   └── Details   - Type-specific details (sealed interface)
//	└── LinkStatus    - Observed runtime state
//	    ├── Kernel    - Live kernel link info (nil if synthetic/not present)
//	    ├── KernelSeen - Whether kernel enumeration found the link
//	    └── PinPresent - Whether the pin path exists on filesystem
//
// # Link Lifecycle Flow
//
// 1. *AttachSpec (e.g., TracepointAttachSpec): User provides validated input
// describing what to attach. Each attach type has its own spec type containing
// the program ID and type-specific parameters.
//
// 2. AttachOutput: Transient result from kernel attach operation. Contains
// kernel-assigned link ID, kernel link info, and pin path. Not stored - just
// passes data from I/O boundary to manager.
//
//	AttachOutput {
//	    LinkID     uint32       // kernel-assigned or synthetic
//	    KernelLink *kernel.Link // nil for synthetic links
//	    PinPath    string       // where link was pinned
//	    Synthetic  bool         // true for perf_event-based links
//	}
//
// 3. LinkRecord: Manager combines *AttachSpec + AttachOutput and stores it in
// the database. The manager constructs the LinkRecord using the attach spec's
// details and the kernel-returned IDs/paths.
//
// 4. LinkStatus: Observed by querying kernel and filesystem. For synthetic
// links (container uprobes), the kernel link is nil and KernelSeen is false.
//
// 5. Link: Combines Record + Status. The coherency and GC systems compare
// these to detect drift and generate remediation actions.
//
// # Synthetic Links
//
// Some attach types (e.g., container uprobes) use perf_event-based mechanisms
// that cannot be pinned and don't have kernel link IDs. For these, bpfman
// generates synthetic link IDs in the range 0x80000000-0xFFFFFFFF to avoid
// collision with real kernel link IDs. The IsSyntheticLinkID() function
// identifies these.
//
// # Key Distinctions
//
// LoadSpec is input (what to load), ProgramRecord is stored output (what was
// loaded). They share some fields but serve different purposes.
//
// Similarly, *AttachSpec is input (what to attach), LinkRecord is stored output
// (what was attached). The AttachOutput bridges the I/O boundary, carrying
// kernel-assigned IDs to the manager for LinkRecord construction.
package bpfman
