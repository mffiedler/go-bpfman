// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright Authors of bpfman

// Double-buffered TC dispatcher.
//
// Derived from tc_dispatcher.bpf.c. The dispatch loop is driven by
// runtime BPF maps instead of .rodata constants, allowing the control
// plane to reorder programs without reloading the dispatcher. TC has
// no metadata to preserve, so .rodata is eliminated entirely.

#include <linux/bpf.h>
#include <linux/in.h>
#include <linux/pkt_cls.h>

#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#define TC_METADATA_SECTION "tc_metadata"
#define TC_DISPATCHER_VERSION 2
#define TC_DISPATCHER_RETVAL 30
#define MAX_DISPATCHER_ACTIONS 10

#ifdef DEBUG
#define bpf_dbg(fmt, ...) bpf_printk(fmt, ##__VA_ARGS__)
#else
#define bpf_dbg(fmt, ...) ((void)0)
#endif

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

__attribute__((noinline)) int prog0(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog1(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog2(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog3(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog4(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog5(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog6(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog7(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog8(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int prog9(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

__attribute__((noinline)) int compat_test(struct __sk_buff *skb) {
  volatile int ret = TC_DISPATCHER_RETVAL;

  if (!skb)
    return TC_ACT_UNSPEC;
  return ret;
}

static __always_inline int call_slot(struct __sk_buff *skb, __u32 slot) {
  switch (slot) {
  case 0:
    return prog0(skb);
  case 1:
    return prog1(skb);
  case 2:
    return prog2(skb);
  case 3:
    return prog3(skb);
  case 4:
    return prog4(skb);
  case 5:
    return prog5(skb);
  case 6:
    return prog6(skb);
  case 7:
    return prog7(skb);
  case 8:
    return prog8(skb);
  case 9:
    return prog9(skb);
  default:
    return TC_ACT_OK;
  }
}

SEC("classifier/dispatcher")
int tc_dispatcher(struct __sk_buff *skb) {
  __u32 key = 0;
  __u32 *gen = bpf_map_lookup_elem(&active_config, &key);
  if (!gen) {
    bpf_dbg("tc_dispatcher: active_config lookup failed");
    return TC_ACT_OK;
  }

  __u32 active_idx = *gen;
  bpf_dbg("tc_dispatcher: active_idx=%u", active_idx);

  struct dispatcher_runtime *cfg =
      bpf_map_lookup_elem(&dispatcher_config, &active_idx);
  if (!cfg) {
    bpf_dbg("tc_dispatcher: dispatcher_config[%u] lookup failed", active_idx);
    return TC_ACT_OK;
  }

  bpf_dbg("tc_dispatcher: num_progs_enabled=%u", cfg->num_progs_enabled);

#pragma unroll
  for (int i = 0; i < MAX_DISPATCHER_ACTIONS; i++) {
    if (i >= cfg->num_progs_enabled)
      break;
    __u32 slot = cfg->run_order[i];
    if (slot >= MAX_DISPATCHER_ACTIONS) {
      bpf_dbg("tc_dispatcher: run_order[%d]=%u exceeds MAX", i, slot);
      break;
    }
    bpf_dbg("tc_dispatcher: calling slot %u at position %d", slot, i);
    int ret = call_slot(skb, slot);
    bpf_dbg("tc_dispatcher: slot %u returned %d, chain_call_actions=0x%x", slot, ret, cfg->chain_call_actions[slot]);
    if (!((1U << (ret + 1)) & cfg->chain_call_actions[slot])) {
      bpf_dbg("tc_dispatcher: returning %d from slot %u", ret, slot);
      return ret;
    }
  }

  bpf_dbg("tc_dispatcher: returning TC_ACT_OK");
  return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";

__uint(dispatcher_version, TC_DISPATCHER_VERSION) SEC(TC_METADATA_SECTION);
