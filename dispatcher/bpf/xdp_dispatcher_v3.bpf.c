// SPDX-License-Identifier: GPL-2.0-only
// Modifications Copyright Authors of bpfman

// Double-buffered XDP dispatcher.
//
// Derived from xdp_dispatcher_v2.bpf.c. The dispatch loop is driven
// by runtime BPF maps instead of .rodata constants, allowing the
// control plane to reorder programs without reloading the dispatcher.
//
// .rodata is retained for XDP metadata (magic, dispatcher_version,
// is_xdp_frags, program_flags, run_prios) -- these are load-time
// constants used for xdp-tools compatibility.

// clang-format off
#include <linux/bpf.h>
#include <linux/in.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
// clang-format on

#define XDP_METADATA_SECTION "xdp_metadata"
#define XDP_DISPATCHER_VERSION 2
#define XDP_DISPATCHER_MAGIC 236
#define XDP_DISPATCHER_RETVAL 31
#define MAX_DISPATCHER_ACTIONS 10

#ifdef DEBUG
#define bpf_dbg(fmt, ...) bpf_printk(fmt, ##__VA_ARGS__)
#else
#define bpf_dbg(fmt, ...) ((void)0)
#endif

// .rodata metadata for xdp-tools compatibility. The dispatch loop
// no longer reads chain_call_actions or num_progs_enabled from here;
// those live in the runtime maps below.
struct xdp_dispatcher_conf {
  __u8 magic;
  __u8 dispatcher_version;
  __u8 num_progs_enabled;
  __u8 is_xdp_frags;
  __u32 chain_call_actions[MAX_DISPATCHER_ACTIONS];
  __u32 run_prios[MAX_DISPATCHER_ACTIONS];
  __u32 program_flags[MAX_DISPATCHER_ACTIONS];
};

static volatile const struct xdp_dispatcher_conf conf = {};

// Runtime dispatch configuration, written by the control plane.
struct dispatcher_runtime {
  __u32 num_progs_enabled;
  __u32 run_order[MAX_DISPATCHER_ACTIONS];
  __u32 chain_call_actions[MAX_DISPATCHER_ACTIONS];
};

// Double-buffer: two config entries, only one active at a time.
struct {
  __uint(type, BPF_MAP_TYPE_ARRAY);
  __uint(max_entries, 2);
  __type(key, __u32);
  __type(value, struct dispatcher_runtime);
} dispatcher_config SEC(".maps");

// Points to the currently active config entry (0 or 1).
struct {
  __uint(type, BPF_MAP_TYPE_ARRAY);
  __uint(max_entries, 1);
  __type(key, __u32);
  __type(value, __u32);
} active_config SEC(".maps");

__attribute__((noinline)) int prog0(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog1(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog2(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog3(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog4(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog5(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog6(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog7(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog8(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int prog9(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

__attribute__((noinline)) int compat_test(struct xdp_md *ctx) {
  volatile int ret = XDP_DISPATCHER_RETVAL;

  if (!ctx)
    return XDP_ABORTED;
  return ret;
}

static __always_inline int call_slot(struct xdp_md *ctx, __u32 slot) {
  switch (slot) {
  case 0:
    return prog0(ctx);
  case 1:
    return prog1(ctx);
  case 2:
    return prog2(ctx);
  case 3:
    return prog3(ctx);
  case 4:
    return prog4(ctx);
  case 5:
    return prog5(ctx);
  case 6:
    return prog6(ctx);
  case 7:
    return prog7(ctx);
  case 8:
    return prog8(ctx);
  case 9:
    return prog9(ctx);
  default:
    return XDP_PASS;
  }
}

SEC("xdp")
int xdp_dispatcher(struct xdp_md *ctx) {
  __u32 key = 0;
  __u32 *gen = bpf_map_lookup_elem(&active_config, &key);
  if (!gen) {
    bpf_dbg("xdp_dispatcher: active_config lookup failed");
    return XDP_PASS;
  }

  __u32 active_idx = *gen;
  bpf_dbg("xdp_dispatcher: active_idx=%u", active_idx);

  struct dispatcher_runtime *cfg =
      bpf_map_lookup_elem(&dispatcher_config, &active_idx);
  if (!cfg) {
    bpf_dbg("xdp_dispatcher: dispatcher_config[%u] lookup failed", active_idx);
    return XDP_PASS;
  }

  bpf_dbg("xdp_dispatcher: num_progs_enabled=%u", cfg->num_progs_enabled);

#pragma unroll
  for (int i = 0; i < MAX_DISPATCHER_ACTIONS; i++) {
    if (i >= cfg->num_progs_enabled)
      break;
    __u32 slot = cfg->run_order[i];
    if (slot >= MAX_DISPATCHER_ACTIONS) {
      bpf_dbg("xdp_dispatcher: run_order[%d]=%u exceeds MAX", i, slot);
      break;
    }
    bpf_dbg("xdp_dispatcher: calling slot %u at position %d", slot, i);
    int ret = call_slot(ctx, slot);
    bpf_dbg("xdp_dispatcher: slot %u returned %d, chain_call_actions=0x%x", slot, ret, cfg->chain_call_actions[slot]);
    if (!((1U << ret) & cfg->chain_call_actions[slot])) {
      bpf_dbg("xdp_dispatcher: returning %d from slot %u", ret, slot);
      return ret;
    }
  }

  /* keep a reference to the compat_test() function so we can use it
   * as an freplace target in xdp_multiprog__check_compat() in libxdp
   */
  if (conf.num_progs_enabled < 11)
    goto out;
  compat_test(ctx);
out:
  bpf_dbg("xdp_dispatcher: returning XDP_PASS");
  return XDP_PASS;
}

char _license[] SEC("license") = "GPL";
__uint(dispatcher_version, XDP_DISPATCHER_VERSION) SEC(XDP_METADATA_SECTION);
