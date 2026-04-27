// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of bpfman

// Trivial helper binary for uprobe/uretprobe e2e tests. The uprobe is
// attached directly to the exported do_work() function in this binary,
// avoiding any dependency on finding the correct libc path (which
// breaks on NixOS, Guix, musl, and other non-standard layouts).

#include <stdlib.h>

// noinline keeps do_work as a real, callable symbol; optimize("O0")
// prevents the malloc(1)/free(p) pair from being elided as dead
// code at the compiler's default optimisation level. With both the
// pair removed and the function body empty, the optimiser would
// also elide main's call to do_work, leaving the uprobe attached
// but never fired.
__attribute__((noinline, optimize("O0")))
void do_work(void) {
	volatile void *p = malloc(1);
	free((void *)p);
}

int main(void) {
	do_work();
	return 0;
}
