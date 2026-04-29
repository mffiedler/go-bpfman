//go:build e2e

package e2e

// e2e_do_work is the cgo'd uprobe attach target shared by the
// uprobe / uretprobe e2e tests. Its address resolves to a real
// symbol in the e2e.test binary's symbol table, and the function
// body contains an unelidable malloc/free pair so the kernel
// uprobe can fire on entry/return.
//
// Lives in a non-_test.go file because go test -c forbids cgo
// in test files; this file is gated by the e2e build tag and
// only compiles into e2e.test.

// #include <stdlib.h>
// __attribute__((noinline, optimize("O0")))
// void e2e_do_work(void) {
//     volatile void *p = malloc(1);
//     free((void *)p);
// }
import "C"

// callDoWork invokes the cgo'd uprobe target. Used by TestMain's
// helper-mode dispatch (see BPFMAN_E2E_MODE=call-malloc).
func callDoWork() {
	C.e2e_do_work()
}
