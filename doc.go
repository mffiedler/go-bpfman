// Package bpfman provides types and interfaces for BPF program management.
// This is the root package containing shared domain types used across
// the client, manager, and server components.
//
// # Type Overview
//
// The core types form a hierarchy reflecting the program lifecycle:
//
//	Program           - The complete domain object combining Spec and Status
//	├── ProgramSpec   - DB-backed desired state (what bpfman manages)
//	│   ├── LoadSpec  - Validated load request (immutable, private fields)
//	│   ├── License/GPLCompatible - Discovered from ELF at load time
//	│   ├── Handles   - Filesystem paths (pin, maps)
//	│   └── Meta      - User-facing metadata (name, owner, labels)
//	└── ProgramStatus - Observed runtime state (kernel + filesystem)
//	    ├── Kernel    - Live kernel program info (nil if not present)
//	    ├── Links     - Attached links with their own spec/status
//	    └── Maps      - Associated kernel maps
//
// # Lifecycle Flow
//
// 1. LoadSpec: User provides validated input describing what to load.
// Created via NewLoadSpec() or builder methods. Immutable after construction.
//
// 2. LoadOutput: Transient result from kernel.Load(). Contains kernel-assigned
// ID, pin paths, and license discovered from ELF. Not stored - just passes
// data from I/O boundary to manager.
//
// 3. ProgramSpec: Manager combines LoadSpec + LoadOutput + user metadata and
// stores it in the database. License and GPLCompatible live here because
// they are discovered properties, not part of the original load request.
//
// 4. ProgramStatus: Observed by querying kernel and filesystem. Represents
// what actually exists now - can diverge from Spec if programs are unloaded
// externally or pin files are deleted.
//
// 5. Program: Combines Spec + Status. The coherency and GC systems compare
// these to detect drift and generate remediation actions.
//
// # Key Distinction
//
// LoadSpec is input (what to load), ProgramSpec is stored output (what was
// loaded). They share some fields but serve different purposes in the
// lifecycle.
package bpfman
