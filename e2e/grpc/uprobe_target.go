//go:build e2e

package grpcparallel

// This file holds a cgo'd C symbol that the uprobe sub-test
// attaches to. Mirrors e2e/uprobe_target.go: a noinline,
// optimize-off function with an unelidable side-effecting body,
// so it resolves to a real ELF symbol in the e2e-grpc.test
// binary's symbol table.
//
// Lives in a non-_test.go file because `go test -c` forbids cgo
// in test files; this file is gated by the e2e build tag and
// only compiles into e2e-grpc.test.

// #include <stdlib.h>
// __attribute__((used, noinline, optimize("O0")))
// void grpc_uprobe_target(void) {
//     volatile void *p = malloc(1);
//     free((void *)p);
// }
import "C"

// uprobeTargetSymbol is the ELF symbol name the cgo target above
// resolves to in the test binary. bpfman attaches a uprobe to
// /proc/self/exe at this symbol's address.
const uprobeTargetSymbol = "grpc_uprobe_target"

// invokeUprobeTarget calls the C function from Go so the linker
// keeps the symbol live. The test never invokes this; the
// indirect reference via callUprobeTargetOnce is enough.
//
//nolint:unused // referenced via callUprobeTargetOnce to retain the symbol.
func invokeUprobeTarget() { C.grpc_uprobe_target() }

// callUprobeTargetOnce stamps invokeUprobeTarget into a package
// variable so the linker treats the function as reachable and
// retains the cgo'd C symbol. The `used` attribute on the C side
// is belt-and-braces; this Go-side reference is the suspenders.
var callUprobeTargetOnce = invokeUprobeTarget
