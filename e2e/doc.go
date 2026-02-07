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
// # Program Types Tested
//
// The test suite covers all supported BPF program types:
//
//   - Tracepoint: kernel tracepoint hooks (sched/sched_switch, etc.)
//   - Kprobe/Kretprobe: kernel function entry and return probes
//   - Uprobe/Uretprobe: userspace function probes (typically libc)
//   - Fentry/Fexit: fast kernel function tracing (requires BTF)
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
