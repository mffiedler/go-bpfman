// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of bpfman

// Trivial helper binary for uprobe/uretprobe e2e tests. The uprobe is
// attached directly to the exported do_work() function in this binary,
// avoiding any dependency on finding the correct libc path (which
// breaks on NixOS, Guix, musl, and other non-standard layouts).

#include <stdlib.h>

__attribute__((noinline))
void do_work(void) {
	volatile void *p = malloc(1);
	free((void *)p);
}

int main(void) {
	do_work();
	return 0;
}
