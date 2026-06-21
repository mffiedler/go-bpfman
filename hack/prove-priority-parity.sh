#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go_bpfman="${GO_BPFMAN:-$repo_root/bin/bpfman}"
rust_bpfman="${BPFMAN_RUST_BINARY:-${RUST_BPFMAN:-$HOME/src/github.com/bpfman/bpfman/target/debug/bpfman}}"
tmpdir="$(mktemp -d)"

links=()
programs=()
ifaces=()

cleanup() {
	local item kind id iface
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

need awk
need clang
need jq
need sudo
need ip

[[ -x "$go_bpfman" ]] || die "Go bpfman binary not executable: $go_bpfman"
[[ -x "$rust_bpfman" ]] || die "Rust bpfman binary not executable: $rust_bpfman"

cat >"$tmpdir/xdp0.c" <<'EOF'
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

SEC("xdp")
int xdp0_prog(struct xdp_md *ctx) { return XDP_PASS; }

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/xdp30.c" <<'EOF'
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

SEC("xdp")
int xdp30_prog(struct xdp_md *ctx) { return XDP_PASS; }

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/tc0.c" <<'EOF'
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

SEC("classifier/tc0_prog")
int tc0_prog(struct __sk_buff *skb) { return TC_ACT_PIPE; }

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/tc30.c" <<'EOF'
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

SEC("classifier/tc30_prog")
int tc30_prog(struct __sk_buff *skb) { return TC_ACT_PIPE; }

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/tcx0.c" <<'EOF'
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

SEC("classifier/tcx0_prog")
int tcx0_prog(struct __sk_buff *skb) { return TC_ACT_PIPE; }

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

cat >"$tmpdir/tcx30.c" <<'EOF'
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

SEC("classifier/tcx30_prog")
int tcx30_prog(struct __sk_buff *skb) { return TC_ACT_PIPE; }

char _license[] SEC("license") = "Dual BSD/GPL";
EOF

compile_obj() {
	local name="$1"
	clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
		-c "$tmpdir/$name.c" -o "$tmpdir/$name.bpf.o"
}

for obj in xdp0 xdp30 tc0 tc30 tcx0 tcx30; do
	compile_obj "$obj"
done

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

setup_veth() {
	local host="$1" peer="${1}p"
	sudo ip link del "$host" >/dev/null 2>&1 || true
	sudo ip link add "$host" type veth peer name "$peer"
	ifaces+=("$host")
	sudo ip link set "$host" up
	sudo ip link set "$peer" up
}

rust_link_field() {
	local link="$1" field="$2"
	local out
	out="$(sudo "$rust_bpfman" get link "$link")"
	awk -v field="$field" '$1 == field ":" { print $2; exit }' <<<"$out"
}

go_link_field() {
	local link="$1" field="$2"
	sudo "$go_bpfman" link get "$link" -o json |
		jq -r --arg field "$field" '.record.details[$field]'
}

assert_eq() {
	local label="$1" got="$2" want="$3"
	if [[ "$got" != "$want" ]]; then
		die "$label: got $got, want $want"
	fi
	printf '  %s = %s\n' "$label" "$got"
}

check_pair() {
	local impl="$1" kind="$2" link0="$3" link30="$4"
	local prio0 pos0 prio30 pos30

	case "$impl" in
	rust)
		prio0="$(rust_link_field "$link0" Priority)"
		pos0="$(rust_link_field "$link0" Position)"
		prio30="$(rust_link_field "$link30" Priority)"
		pos30="$(rust_link_field "$link30" Position)"
		;;
	go)
		prio0="$(go_link_field "$link0" priority)"
		pos0="$(go_link_field "$link0" position)"
		prio30="$(go_link_field "$link30" priority)"
		pos30="$(go_link_field "$link30" position)"
		;;
	*)
		die "unknown implementation $impl"
		;;
	esac

	printf '%s %s priorities 0 then 30\n' "$impl" "$kind"
	assert_eq "$impl $kind priority-0 stored priority" "$prio0" 0
	assert_eq "$impl $kind priority-0 position" "$pos0" 0
	assert_eq "$impl $kind priority-30 stored priority" "$prio30" 30
	assert_eq "$impl $kind priority-30 position" "$pos30" 1
}

run_rust_xdp() {
	local iface="ppxrh" out p0 p30 l0 l30
	setup_veth "$iface"
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/xdp0.bpf.o" --programs xdp:xdp0_prog)"
	p0="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$p0")
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/xdp30.bpf.o" --programs xdp:xdp30_prog)"
	p30="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$p30")
	out="$(sudo "$rust_bpfman" attach "$p0" xdp --iface "$iface" --priority 0)"
	l0="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$l0")
	out="$(sudo "$rust_bpfman" attach "$p30" xdp --iface "$iface" --priority 30)"
	l30="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$l30")
	check_pair rust xdp "$l0" "$l30"
}

run_go_xdp() {
	local iface="ppxgh" out p0 p30 l0 l30
	setup_veth "$iface"
	out="$(sudo "$go_bpfman" program load file "$tmpdir/xdp0.bpf.o" --programs xdp:xdp0_prog -o json)"
	p0="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$p0")
	out="$(sudo "$go_bpfman" program load file "$tmpdir/xdp30.bpf.o" --programs xdp:xdp30_prog -o json)"
	p30="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$p30")
	out="$(sudo "$go_bpfman" link attach xdp "$p0" "$iface" --priority 0 -o json)"
	l0="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$l0")
	out="$(sudo "$go_bpfman" link attach xdp "$p30" "$iface" --priority 30 -o json)"
	l30="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$l30")
	check_pair go xdp "$l0" "$l30"
}

run_rust_tc() {
	local iface="pptrh" out p0 p30 l0 l30
	setup_veth "$iface"
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/tc0.bpf.o" --programs tc:tc0_prog)"
	p0="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$p0")
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/tc30.bpf.o" --programs tc:tc30_prog)"
	p30="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$p30")
	out="$(sudo "$rust_bpfman" attach "$p0" tc --direction ingress --iface "$iface" --priority 0)"
	l0="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$l0")
	out="$(sudo "$rust_bpfman" attach "$p30" tc --direction ingress --iface "$iface" --priority 30)"
	l30="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$l30")
	check_pair rust tc "$l0" "$l30"
}

run_go_tc() {
	local iface="pptgh" out p0 p30 l0 l30
	setup_veth "$iface"
	out="$(sudo "$go_bpfman" program load file "$tmpdir/tc0.bpf.o" --programs tc:tc0_prog -o json)"
	p0="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$p0")
	out="$(sudo "$go_bpfman" program load file "$tmpdir/tc30.bpf.o" --programs tc:tc30_prog -o json)"
	p30="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$p30")
	out="$(sudo "$go_bpfman" link attach tc "$p0" "$iface" ingress --priority 0 -o json)"
	l0="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$l0")
	out="$(sudo "$go_bpfman" link attach tc "$p30" "$iface" ingress --priority 30 -o json)"
	l30="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$l30")
	check_pair go tc "$l0" "$l30"
}

run_rust_tcx() {
	local iface="ppcrh" out p0 p30 l0 l30
	setup_veth "$iface"
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/tcx0.bpf.o" --programs tcx:tcx0_prog)"
	p0="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$p0")
	out="$(sudo "$rust_bpfman" load file --path "$tmpdir/tcx30.bpf.o" --programs tcx:tcx30_prog)"
	p30="$(printf '%s\n' "$out" | rust_program_id)"
	programs+=("rust:$p30")
	out="$(sudo "$rust_bpfman" attach "$p0" tcx --direction ingress --iface "$iface" --priority 0)"
	l0="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$l0")
	out="$(sudo "$rust_bpfman" attach "$p30" tcx --direction ingress --iface "$iface" --priority 30)"
	l30="$(printf '%s\n' "$out" | rust_link_id)"
	links+=("rust:$l30")
	check_pair rust tcx "$l0" "$l30"
}

run_go_tcx() {
	local iface="ppcgh" out p0 p30 l0 l30
	setup_veth "$iface"
	out="$(sudo "$go_bpfman" program load file "$tmpdir/tcx0.bpf.o" --programs tcx:tcx0_prog -o json)"
	p0="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$p0")
	out="$(sudo "$go_bpfman" program load file "$tmpdir/tcx30.bpf.o" --programs tcx:tcx30_prog -o json)"
	p30="$(printf '%s\n' "$out" | go_program_id)"
	programs+=("go:$p30")
	out="$(sudo "$go_bpfman" link attach tcx "$p0" "$iface" ingress --priority 0 -o json)"
	l0="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$l0")
	out="$(sudo "$go_bpfman" link attach tcx "$p30" "$iface" ingress --priority 30 -o json)"
	l30="$(printf '%s\n' "$out" | go_link_id)"
	links+=("go:$l30")
	check_pair go tcx "$l0" "$l30"
}

printf 'Priority parity proof\n'
printf '  Go bpfman:   %s\n' "$go_bpfman"
printf '  Rust bpfman: %s\n' "$rust_bpfman"

run_rust_xdp
run_go_xdp
run_rust_tc
run_go_tc
run_rust_tcx
run_go_tcx

printf 'priority parity proof passed\n'
