#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go_bpfman="${GO_BPFMAN:-$repo_root/bin/bpfman}"
rust_bpfman="${RUST_BPFMAN:-$HOME/src/github.com/bpfman/bpfman/target/debug/bpfman}"
tmpdir="$(mktemp -d)"

links=()
programs=()
netns=()
ifaces=()
rules=()
setup_host=
setup_ns=
setup_host_ip=

cleanup() {
	local item kind id
	set +e
	for item in "${links[@]}"; do
		kind="${item%%:*}"
		id="${item#*:}"
		case "$kind" in
		go) sudo "$go_bpfman" link detach "$id" >/dev/null 2>&1 ;;
		rust) sudo "$rust_bpfman" detach "$id" >/dev/null 2>&1 ;;
		esac
	done
	for item in "${programs[@]}"; do
		kind="${item%%:*}"
		id="${item#*:}"
		case "$kind" in
		go) sudo "$go_bpfman" program unload "$id" >/dev/null 2>&1 ;;
		rust) sudo "$rust_bpfman" unload "$id" >/dev/null 2>&1 ;;
		esac
	done
	for iface in "${ifaces[@]}"; do
		sudo ip link del "$iface" >/dev/null 2>&1
	done
	for rule in "${rules[@]}"; do
		sudo ip rule del pref 99 to "$rule" lookup main >/dev/null 2>&1
	done
	for ns in "${netns[@]}"; do
		sudo ip netns del "$ns" >/dev/null 2>&1
	done
	rm -rf "$tmpdir"
}
trap cleanup EXIT

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

need clang
need jq
need sudo
need ip
need ping
need bpftool

[[ -x "$go_bpfman" ]] || die "Go bpfman binary not executable: $go_bpfman"
[[ -x "$rust_bpfman" ]] || die "Rust bpfman binary not executable: $rust_bpfman"

cat >"$tmpdir/xdp_drop.c" <<'EOF'
// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define IPPROTO_ICMP 1
#define ICMP_ECHO 8

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, 1);
} xdp_drop_count SEC(".maps");

static __always_inline int is_echo_request(void *data, void *data_end) {
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end || eth->h_proto != __bpf_htons(ETH_P_IP))
		return 0;
	struct iphdr *iph = (void *)(eth + 1);
	if ((void *)(iph + 1) > data_end || iph->protocol != IPPROTO_ICMP)
		return 0;
	__u8 *icmp_type = (void *)iph + (iph->ihl * 4);
	if ((void *)(icmp_type + 1) > data_end)
		return 0;
	return *icmp_type == ICMP_ECHO;
}

SEC("xdp")
int xdp_drop_prog(struct xdp_md *ctx) {
	void *data = (void *)(long)ctx->data;
	void *data_end = (void *)(long)ctx->data_end;
	if (is_echo_request(data, data_end)) {
		__u32 key = 0;
		__u64 *val = bpf_map_lookup_elem(&xdp_drop_count, &key);
		if (val)
			(*val)++;
	}
	return XDP_DROP;
}

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/xdp_pass.c" <<'EOF'
// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define IPPROTO_ICMP 1
#define ICMP_ECHO 8

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, 1);
} xdp_pass_count SEC(".maps");

static __always_inline int is_echo_request(void *data, void *data_end) {
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end || eth->h_proto != __bpf_htons(ETH_P_IP))
		return 0;
	struct iphdr *iph = (void *)(eth + 1);
	if ((void *)(iph + 1) > data_end || iph->protocol != IPPROTO_ICMP)
		return 0;
	__u8 *icmp_type = (void *)iph + (iph->ihl * 4);
	if ((void *)(icmp_type + 1) > data_end)
		return 0;
	return *icmp_type == ICMP_ECHO;
}

SEC("xdp")
int xdp_pass_prog(struct xdp_md *ctx) {
	void *data = (void *)(long)ctx->data;
	void *data_end = (void *)(long)ctx->data_end;
	if (is_echo_request(data, data_end)) {
		__u32 key = 0;
		__u64 *val = bpf_map_lookup_elem(&xdp_pass_count, &key);
		if (val)
			(*val)++;
	}
	return XDP_PASS;
}

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/tc_unspec.c" <<'EOF'
// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define IPPROTO_ICMP 1
#define ICMP_ECHO 8

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, 1);
} tc_unspec_count SEC(".maps");

static __always_inline int is_echo_request(void *data, void *data_end) {
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end || eth->h_proto != __bpf_htons(ETH_P_IP))
		return 0;
	struct iphdr *iph = (void *)(eth + 1);
	if ((void *)(iph + 1) > data_end || iph->protocol != IPPROTO_ICMP)
		return 0;
	__u8 *icmp_type = (void *)iph + (iph->ihl * 4);
	if ((void *)(icmp_type + 1) > data_end)
		return 0;
	return *icmp_type == ICMP_ECHO;
}

SEC("classifier/tc_unspec_prog")
int tc_unspec_prog(struct __sk_buff *skb) {
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	if (is_echo_request(data, data_end)) {
		__u32 key = 0;
		__u64 *val = bpf_map_lookup_elem(&tc_unspec_count, &key);
		if (val)
			(*val)++;
	}
	return TC_ACT_UNSPEC;
}

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/tc_ok.c" <<'EOF'
// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define IPPROTO_ICMP 1
#define ICMP_ECHO 8

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, 1);
} tc_ok_count SEC(".maps");

static __always_inline int is_echo_request(void *data, void *data_end) {
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end || eth->h_proto != __bpf_htons(ETH_P_IP))
		return 0;
	struct iphdr *iph = (void *)(eth + 1);
	if ((void *)(iph + 1) > data_end || iph->protocol != IPPROTO_ICMP)
		return 0;
	__u8 *icmp_type = (void *)iph + (iph->ihl * 4);
	if ((void *)(icmp_type + 1) > data_end)
		return 0;
	return *icmp_type == ICMP_ECHO;
}

SEC("classifier/tc_ok_prog")
int tc_ok_prog(struct __sk_buff *skb) {
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	if (is_echo_request(data, data_end)) {
		__u32 key = 0;
		__u64 *val = bpf_map_lookup_elem(&tc_ok_count, &key);
		if (val)
			(*val)++;
	}
	return TC_ACT_OK;
}

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

compile_obj() {
	local name="$1"
	clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
		-c "$tmpdir/$name.c" -o "$tmpdir/$name.bpf.o"
}

compile_obj xdp_drop
compile_obj xdp_pass
compile_obj tc_unspec
compile_obj tc_ok

map_value() {
	local pin="$1"
	local raw
	raw="$(sudo bpftool -j map dump pinned "$pin" |
		jq -r '.[0].value | if type == "array" then .[0] else . end')"
	printf '%d\n' "$((raw))"
}

assert_delta() {
	local label="$1" before="$2" after="$3" want="$4"
	local got=$((after - before))
	if [[ "$got" -ne "$want" ]]; then
		die "$label delta = $got, want $want (before=$before after=$after)"
	fi
	printf '  %s delta=%d\n' "$label" "$got"
}

setup_net() {
	local tag="$1" third_octet="$2"
	local host="bp${tag}h" peer="bp${tag}p" ns="bp${tag}n"
	local host_ip="198.51.${third_octet}.1" peer_ip="198.51.${third_octet}.2"

	sudo ip link del "$host" >/dev/null 2>&1 || true
	sudo ip netns del "$ns" >/dev/null 2>&1 || true
	sudo ip netns add "$ns"
	netns+=("$ns")
	sudo ip link add "$host" type veth peer name "$peer"
	ifaces+=("$host")
	sudo ip link set "$peer" netns "$ns"
	sudo ip addr add "$host_ip/24" dev "$host"
	sudo ip link set "$host" up
	sudo ip netns exec "$ns" ip addr add "$peer_ip/24" dev "$peer"
	sudo ip netns exec "$ns" ip link set "$peer" up
	sudo ip rule del pref 99 to "$peer_ip/32" lookup main >/dev/null 2>&1 || true
	sudo ip rule add pref 99 to "$peer_ip/32" lookup main
	rules+=("$peer_ip/32")
	sudo ip netns exec "$ns" ping -c 1 -W 1 "$host_ip" >/dev/null

	setup_host="$host"
	setup_ns="$ns"
	setup_host_ip="$host_ip"
}

rust_program_id() {
	awk '/Program ID:/ { print $3; exit }'
}

rust_link_id() {
	awk '/Link ID:/ { print $3; exit }'
}

go_program_id() {
	jq -r '.programs[0].record.program_id'
}

go_link_id() {
	jq -r '.record.id'
}

run_rust_xdp() {
	local host ns host_ip out drop_prog pass_prog drop_link pass_link
	setup_net rx 81
	host="$setup_host"
	ns="$setup_ns"
	host_ip="$setup_host_ip"
	printf 'Rust XDP: %s in %s\n' "$host" "$ns"

	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/xdp_drop.bpf.o" --programs xdp:xdp_drop_prog)"
	drop_prog="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$drop_prog")
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/xdp_pass.bpf.o" --programs xdp:xdp_pass_prog)"
	pass_prog="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$pass_prog")

	out="$(sudo "$rust_bpfman" attach "$drop_prog" xdp --iface "$host" --priority 50 --proceed-on drop)"
	drop_link="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$drop_link")
	out="$(sudo "$rust_bpfman" attach "$pass_prog" xdp --iface "$host" --priority 100)"
	pass_link="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$pass_link")

	local before_drop before_pass after_drop after_pass
	before_drop="$(map_value "/run/bpfman/fs/maps/$drop_prog/xdp_drop_count")"
	before_pass="$(map_value "/run/bpfman/fs/maps/$pass_prog/xdp_pass_count")"
	sudo ip netns exec "$ns" ping -c 3 -W 1 "$host_ip" >/dev/null
	after_drop="$(map_value "/run/bpfman/fs/maps/$drop_prog/xdp_drop_count")"
	after_pass="$(map_value "/run/bpfman/fs/maps/$pass_prog/xdp_pass_count")"
	assert_delta 'xdp_drop_count' "$before_drop" "$after_drop" 3
	assert_delta 'xdp_pass_count' "$before_pass" "$after_pass" 3
}

run_rust_tc() {
	local host ns host_ip out unspec_prog ok_prog unspec_link ok_link
	setup_net rt 82
	host="$setup_host"
	ns="$setup_ns"
	host_ip="$setup_host_ip"
	printf 'Rust TC: %s in %s\n' "$host" "$ns"

	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/tc_unspec.bpf.o" --programs tc:tc_unspec_prog)"
	unspec_prog="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$unspec_prog")
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/tc_ok.bpf.o" --programs tc:tc_ok_prog)"
	ok_prog="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$ok_prog")

	out="$(sudo "$rust_bpfman" attach "$unspec_prog" tc --direction ingress --iface "$host" --priority 50 --proceed-on unspec)"
	unspec_link="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$unspec_link")
	out="$(sudo "$rust_bpfman" attach "$ok_prog" tc --direction ingress --iface "$host" --priority 100)"
	ok_link="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$ok_link")

	local before_unspec before_ok after_unspec after_ok
	before_unspec="$(map_value "/run/bpfman/fs/maps/$unspec_prog/tc_unspec_count")"
	before_ok="$(map_value "/run/bpfman/fs/maps/$ok_prog/tc_ok_count")"
	sudo ip netns exec "$ns" ping -c 3 -W 1 "$host_ip" >/dev/null
	after_unspec="$(map_value "/run/bpfman/fs/maps/$unspec_prog/tc_unspec_count")"
	after_ok="$(map_value "/run/bpfman/fs/maps/$ok_prog/tc_ok_count")"
	assert_delta 'tc_unspec_count' "$before_unspec" "$after_unspec" 3
	assert_delta 'tc_ok_count' "$before_ok" "$after_ok" 3
}

run_go_xdp() {
	local host ns host_ip out drop_prog pass_prog drop_link pass_link
	setup_net gx 83
	host="$setup_host"
	ns="$setup_ns"
	host_ip="$setup_host_ip"
	printf 'Go XDP: %s in %s\n' "$host" "$ns"

	out="$(sudo "$go_bpfman" program load file "$tmpdir/xdp_drop.bpf.o" --programs xdp:xdp_drop_prog -o json)"
	drop_prog="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$drop_prog")
	out="$(sudo "$go_bpfman" program load file "$tmpdir/xdp_pass.bpf.o" --programs xdp:xdp_pass_prog -o json)"
	pass_prog="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$pass_prog")

	out="$(sudo "$go_bpfman" link attach xdp "$drop_prog" "$host" --priority 50 --proceed-on drop -o json)"
	drop_link="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$drop_link")
	out="$(sudo "$go_bpfman" link attach xdp "$pass_prog" "$host" --priority 100 -o json)"
	pass_link="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$pass_link")

	local before_drop before_pass after_drop after_pass
	before_drop="$(map_value "/run/bpfman/fs/maps/$drop_prog/xdp_drop_count")"
	before_pass="$(map_value "/run/bpfman/fs/maps/$pass_prog/xdp_pass_count")"
	sudo ip netns exec "$ns" ping -c 3 -W 1 "$host_ip" >/dev/null
	after_drop="$(map_value "/run/bpfman/fs/maps/$drop_prog/xdp_drop_count")"
	after_pass="$(map_value "/run/bpfman/fs/maps/$pass_prog/xdp_pass_count")"
	assert_delta 'xdp_drop_count' "$before_drop" "$after_drop" 3
	assert_delta 'xdp_pass_count' "$before_pass" "$after_pass" 3
}

run_go_tc() {
	local host ns host_ip out unspec_prog ok_prog unspec_link ok_link
	setup_net gt 84
	host="$setup_host"
	ns="$setup_ns"
	host_ip="$setup_host_ip"
	printf 'Go TC: %s in %s\n' "$host" "$ns"

	out="$(sudo "$go_bpfman" program load file "$tmpdir/tc_unspec.bpf.o" --programs tc:tc_unspec_prog -o json)"
	unspec_prog="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$unspec_prog")
	out="$(sudo "$go_bpfman" program load file "$tmpdir/tc_ok.bpf.o" --programs tc:tc_ok_prog -o json)"
	ok_prog="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$ok_prog")

	out="$(sudo "$go_bpfman" link attach tc "$unspec_prog" "$host" ingress --priority 50 --proceed-on unspec -o json)"
	unspec_link="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$unspec_link")
	out="$(sudo "$go_bpfman" link attach tc "$ok_prog" "$host" ingress --priority 100 -o json)"
	ok_link="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$ok_link")

	local before_unspec before_ok after_unspec after_ok
	before_unspec="$(map_value "/run/bpfman/fs/maps/$unspec_prog/tc_unspec_count")"
	before_ok="$(map_value "/run/bpfman/fs/maps/$ok_prog/tc_ok_count")"
	sudo ip netns exec "$ns" ping -c 3 -W 1 "$host_ip" >/dev/null
	after_unspec="$(map_value "/run/bpfman/fs/maps/$unspec_prog/tc_unspec_count")"
	after_ok="$(map_value "/run/bpfman/fs/maps/$ok_prog/tc_ok_count")"
	assert_delta 'tc_unspec_count' "$before_unspec" "$after_unspec" 3
	assert_delta 'tc_ok_count' "$before_ok" "$after_ok" 3
}

printf 'Proving dispatcher proceed-on parity\n'
printf '  Go bpfman:   %s\n' "$go_bpfman"
printf '  Rust bpfman: %s\n' "$rust_bpfman"

run_rust_xdp
run_rust_tc
run_go_xdp
run_go_tc

printf 'dispatcher parity proof passed\n'
