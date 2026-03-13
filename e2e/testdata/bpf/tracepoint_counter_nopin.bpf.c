// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Variant of tracepoint_counter with map pinning disabled.
#define BPF_MAP_PINNING LIBBPF_PIN_NONE
#include "tracepoint_counter.bpf.c"
