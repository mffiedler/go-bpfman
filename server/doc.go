// Package server implements the bpfman gRPC daemon.
//
// # Overview
//
// The server exposes BPF program lifecycle operations over gRPC,
// translating protobuf requests into domain types and delegating to
// the manager (manager/) for orchestration. It listens on a Unix
// domain socket and optionally on TCP for remote access.
//
// The [Run] function is the main entry point for daemon mode. It
// assembles the full dependency graph (store, kernel adapter,
// filesystem context, image puller, manager) and starts serving.
// [New] provides a lower-level constructor for callers that supply
// their own dependencies, primarily used in tests.
//
// # RPC Methods
//
// The server implements the bpfman.v1.Bpfman service defined in
// server/pb/:
//
//   - Load: load a BPF program from bytecode file or OCI image
//   - Unload: remove a loaded program from kernel and store
//   - Attach: attach a loaded program to a hook point
//   - Detach: detach a program from its hook point
//   - List: enumerate programs with optional metadata filtering
//   - Get: retrieve a single program by ID
//   - PullBytecode: pre-pull an OCI image without loading
//
// Each handler converts protobuf types to domain types (convert.go),
// calls the appropriate manager method, and converts the result back
// to protobuf.
//
// # Interceptors
//
// Two gRPC unary interceptors are chained on every request:
//
//  1. Logging interceptor: assigns a monotonic operation ID to each
//     request and stores it in context for structured log correlation.
//
//  2. Lock interceptor: acquires the global file-based writer lock
//     (lock/) for mutating methods (Load, Unload, Attach, Detach).
//     Read-only methods (List, Get, PullBytecode) bypass the lock.
//     The lock scope is stashed in context so handlers can pass it to
//     manager methods that need it.
//
// Handlers also hold a sync.RWMutex (mu) for in-process serialisation
// of manager access.
//
// # Startup Sequence
//
// [Run] performs the following before accepting requests:
//
//  1. Open SQLite store at the layout's database path
//  2. Create the cilium/ebpf kernel adapter
//  3. Ensure runtime directories and bpffs mount via bpfmanfs/runtime
//  4. Configure signature verification from config
//  5. Create the OCI image puller
//  6. Create the manager with all dependencies
//  7. Optionally start the CSI driver for Kubernetes map exposure
//  8. Optionally start a pprof HTTP server
//  9. Run GC to reconcile stale state from previous runs
//  10. Start serving on Unix socket (and optionally TCP)
//
// # Graceful Shutdown
//
// When the context passed to [Run] is cancelled, the server performs
// an orderly shutdown: the gRPC server drains in-flight requests via
// GracefulStop, the CSI driver (if running) is stopped, and the pprof
// server (if running) is closed.
//
// # CSI Integration
//
// When CSISupport is enabled in [RunConfig], the server starts a
// Kubernetes CSI driver (csi/) alongside the gRPC server. The CSI
// driver shares the manager instance for program lookups, enabling
// pods to access BPF maps via CSI volumes.
package server
