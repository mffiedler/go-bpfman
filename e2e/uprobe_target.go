//go:build e2e

package e2e

// This file holds cgo'd symbols that the uprobe / uretprobe e2e
// tests attach to. Each symbol is named for what it does, so a
// future sibling (e.g. one that calls read() instead of malloc())
// can be added without retrofitting a "target" / "_2" suffix
// onto the existing name.
//
// All such functions:
//   - are declared with __attribute__((noinline, optimize("O0")))
//     so the optimiser doesn't reduce the body to nothing and
//     then inline away every caller;
//   - have an unelidable side-effecting body (a syscall, a
//     malloc/free pair, an atomic store) so the function has
//     real instructions for the kernel uprobe to fire on;
//   - resolve to real symbols in the e2e.test binary's symbol
//     table (the binary is not built with -s -w).
//
// Lives in a non-_test.go file because go test -c forbids cgo
// in test files; this file is gated by the e2e build tag and
// only compiles into e2e.test.

// #include <stdlib.h>
// __attribute__((noinline, optimize("O0")))
// void e2e_uprobe_call_malloc(void) {
//     volatile void *p = malloc(1);
//     free((void *)p);
// }
import "C"

// invokeUprobeCallMalloc calls the cgo'd e2e_uprobe_call_malloc
// function, firing whichever kernel uprobe (or uretprobe) is
// attached to the symbol. Used by TestMain's helper-mode dispatch
// (see BPFMAN_E2E_MODE=uprobe-trigger-call-malloc).
func invokeUprobeCallMalloc() {
	C.e2e_uprobe_call_malloc()
}
