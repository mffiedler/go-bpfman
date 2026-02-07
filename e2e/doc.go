//go:build e2e

// Package e2e provides end-to-end tests for BPF program lifecycle
// operations against a real kernel.
//
// # Overview
//
// These tests verify the complete load/attach/detach/unload cycle for
// each supported BPF program type. They exercise the full stack from
// OCI image pulling through kernel attachment and cleanup.
//
// Tests require root privileges and are excluded from normal builds
// via the e2e build tag. Run with:
//
//	make test-e2e
//
// # Test Environment
//
// Each test creates an isolated [TestEnv] with:
//
//   - Unique runtime directory in /tmp/bpfman-e2e-<pid>-<testname>/
//   - Fresh SQLite database
//   - Dedicated bpffs mount
//   - Independent manager instance
//
// This isolation enables parallel test execution. The environment is
// automatically cleaned up via t.Cleanup, including unmounting bpffs
// and removing all temporary directories.
//
// Network tests (XDP, TC, TCX) use [NewTestInterface] to create a
// dedicated dummy interface per test, avoiding contention on shared
// interfaces like loopback. All tests call t.Parallel().
//
// # Test Descriptions
//
// ## Tracepoint Tests
//
// [TestTracepoint_LoadAttachDetachUnload] tests the full lifecycle of
// a tracepoint program. Loads go-tracepoint-counter from OCI, attaches
// to syscalls/sys_enter_kill, verifies link properties including group
// and name, then detaches and unloads. No traffic generation required.
//
// ## Kprobe Tests
//
// [TestKprobe_LoadAttachDetachUnload] tests the full lifecycle of a
// kprobe program. Loads go-kprobe-counter from OCI, attaches to the
// try_to_wake_up kernel function, verifies link properties including
// function name and offset, then detaches and unloads.
//
// [TestKretprobe_LoadAttachDetachUnload] tests the full lifecycle of
// a kretprobe program. Uses the same image as kprobe but loads as
// kretprobe type. Attaches to try_to_wake_up return, verifies the
// Retprobe flag is set in link details.
//
// ## Uprobe Tests
//
// [TestUprobe_LoadAttachDetachUnload] tests the full lifecycle of a
// uprobe program. Loads go-uprobe-counter from OCI, attaches to malloc
// in libc, verifies link properties including target binary and
// function name, then detaches and unloads.
//
// [TestUretprobe_LoadAttachDetachUnload] tests the full lifecycle of
// a uretprobe program. Uses the same image as uprobe but loads as
// uretprobe type. Attaches to malloc return in libc, verifies the
// Retprobe flag is set in link details.
//
// ## Tracing Tests (BTF Required)
//
// [TestFentry_LoadAttachDetachUnload] tests the full lifecycle of a
// fentry program. Requires BTF support. Loads fentry.bpf.o from local
// bytecode, attaches to do_unlinkat kernel function entry, verifies
// link properties. Skipped if BTF unavailable.
//
// [TestFexit_LoadAttachDetachUnload] tests the full lifecycle of a
// fexit program. Requires BTF support. Loads fentry.bpf.o from local
// bytecode, attaches to do_unlinkat kernel function exit, verifies
// link properties. Skipped if BTF unavailable.
//
// ## Network Tests
//
// Each network test creates a dedicated dummy interface via
// [NewTestInterface], enabling parallel execution.
//
// [TestXDP_LoadAttachDetachUnload] tests the full lifecycle of an XDP
// program. Loads xdp_pass from OCI, attaches to a dummy interface
// using a dispatcher for multi-program support. Verifies dispatcher ID
// and revision in link details.
//
// [TestTC_LoadAttachDetachUnload] tests the full lifecycle of a TC
// program. Loads go-tc-counter from OCI, attaches to ingress with
// priority 50 using a dispatcher. Verifies the TC filter is visible
// via tc(8) tooling and netlink. Confirms filter removal after detach.
//
// [TestTCX_LoadAttachDetachUnload] tests the full lifecycle of a TCX
// program. Requires kernel 6.6+. Loads go-tc-counter from OCI, attaches
// to ingress with priority 50 using native kernel multi-program support
// (no dispatcher). Verifies link properties including interface and
// direction.
//
// ## Metadata Tests
//
// [TestLoadWithMetadataAndGlobalData] verifies that user-supplied
// metadata and global data are stored and returned correctly through
// the full stack. Loads xdp_pass with custom metadata labels and
// global data bytes, verifies they are returned by Get and List
// operations. Does not attach to an interface.
//
// # Program Types Tested
//
// The test suite covers all supported BPF program types:
//
//   - Tracepoint: kernel tracepoint hooks (syscalls/sys_enter_kill)
//   - Kprobe/Kretprobe: kernel function entry and return probes (try_to_wake_up)
//   - Uprobe/Uretprobe: userspace function probes (malloc in libc)
//   - Fentry/Fexit: fast kernel function tracing (do_unlinkat, requires BTF)
//   - XDP: network ingress via dispatcher programs
//   - TC: traffic control via dispatcher programs
//   - TCX: native kernel multi-program TC (requires kernel 6.6+)
//
// Each test follows the same pattern: load from OCI image or bytecode
// file, verify program properties, attach to a hook point, verify link
// properties, detach, unload, and confirm clean state.
//
// # Prerequisite Helpers
//
// The package provides helpers to skip tests when prerequisites are
// not met:
//
//   - [RequireRoot]: fails if not running as root
//   - [RequireBTF]: fails if /sys/kernel/btf/vmlinux is missing
//   - [RequireKernelFunction]: fails if function not in /proc/kallsyms
//   - [RequireKernelVersion]: fails if kernel is below a version
//   - [RequireTracepoint]: fails if tracepoint does not exist
//   - [RequireTC]: fails if tc command (iproute2) is not available
//
// # Bytecode Sources
//
// Tests pull BPF bytecode from OCI container images hosted at
// quay.io/bpfman-bytecode/. The image puller uses the same code path
// as production, including optional signature verification (disabled
// in tests). Some tests use local bytecode files from the
// integration-tests/bytecode/ directory.
//
// # Stale Directory Cleanup
//
// TestMain runs [cleanupStaleTestDirs] to remove leftover directories
// from crashed test runs. It identifies stale directories by checking
// if the PID in the directory name corresponds to a running process.
//
// # Logging
//
// Set the BPFMAN_LOG environment variable to enable debug output:
//
//	BPFMAN_LOG=debug make test-e2e
//	BPFMAN_LOG=info,store=debug make test-e2e
package e2e
