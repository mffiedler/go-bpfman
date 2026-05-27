// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright Authors of bpfman

// Mixed-type multi-program counter object for the .bpfman script
// path of TestMultiProgMixed_LoadAttachDetachUnload. Variant of
// multi_prog_mixed_counter.bpf.o specialised for the script's
// concurrency-isolation shape:
//
//   - the tracepoint program filters on `expected_pid` AND on the
//     exact relative filename "unlinkat-target" that the
//     unlinkat-fire worker creates in its per-script tempdir, so
//     concurrent script instances do not cross-count;
//   - the kprobe/kretprobe programs attach to a leased e2e kfunc
//     slot, so the slot symbol itself is the isolation boundary and
//     no PID filter is needed.
//
// The Go-test path of TestMultiProgMixed_LoadAttachDetachUnload
// still uses multi_prog_mixed_counter.bpf.o (PID-only filter, no
// filename check, no slot-symbol assumption) because its workload
// uses arbitrary filenames and its kprobe/kretprobe attach to
// do_unlinkat rather than a leased slot. Keeping the two .bpf.o
// objects separate avoids each path having to know about the
// other's filter assumptions.
//
// Each program increments its counter by its own per-program
// `weight_X` global on every matching event. Tests pass test-unique
// weights so the final counter value is a verifiable function of
// (events x weight), not just an event tally.

#include "counter_common.bpf.h"
#include <bpf/bpf_core_read.h>

#define UNLINKAT_TARGET "unlinkat-target"
#define UNLINKAT_TARGET_LEN (sizeof(UNLINKAT_TARGET) - 1)

struct trace_event_raw_sys_enter {
	__u64 unused;
	long id;
	unsigned long args[6];
};

volatile const __u32 expected_pid = 0;
volatile const __u64 weight_tp = 0;
volatile const __u64 weight_kp = 0;
volatile const __u64 weight_krp = 0;

COUNTER_MAP(mtp_count);
COUNTER_MAP(mkp_count);
COUNTER_MAP(mkrp_count);

#define SLOT_COUNTER_PROG(prog_name, map_name, weight) \
	int prog_name(void *ctx) { \
		__u32 key = 0; \
		__u64 *val = bpf_map_lookup_elem(&map_name, &key); \
		if (val) \
			__sync_fetch_and_add(val, weight); \
		return 0; \
	}

SEC("tracepoint/mixed_tp")
int mixed_tp(struct trace_event_raw_sys_enter *ctx)
{
	char filename[sizeof(UNLINKAT_TARGET)];
	const char *user_filename = (const char *)ctx->args[1];

	if ((bpf_get_current_pid_tgid() >> 32) != expected_pid)
		return 0;
	if (bpf_probe_read_user_str(filename, sizeof(filename), user_filename) !=
	    sizeof(filename))
		return 0;

#pragma unroll
	for (int i = 0; i < UNLINKAT_TARGET_LEN; i++) {
		if (filename[i] != UNLINKAT_TARGET[i])
			return 0;
	}
	if (filename[UNLINKAT_TARGET_LEN] != '\0')
		return 0;

	__u32 key = 0;
	__u64 *val = bpf_map_lookup_elem(&mtp_count, &key);
	if (val)
		__sync_fetch_and_add(val, weight_tp);
	return 0;
}

SEC("kprobe/mixed_kp")
SLOT_COUNTER_PROG(mixed_kp, mkp_count, weight_kp)

SEC("kretprobe/mixed_krp")
SLOT_COUNTER_PROG(mixed_krp, mkrp_count, weight_krp)

char _license[] SEC("license") = "Dual BSD/GPL";
