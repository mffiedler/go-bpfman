#!/usr/bin/env bash
#
# Manual formatter fixture (bash) for the Rust bpfman implementation:
# load and attach one program of each attachable kind against the Rust
# CLI, then print the rendered output of program/link get and the lists
# so the formatting can be compared side by side with the Go fixture
# (program-load.sh).
#
# The Rust CLI has no JSON output, so program and link ids are scraped
# from the plain-text tables ("Program ID:" in the Kernel State table
# after load, "Link ID:" in the Bpfman State table after attach).
#
# Command-shape differences from the Go CLI:
#   load:   bpfman load file --path FILE --programs TYPE:NAME
#   attach: bpfman attach PROG_ID KIND --flags   (no "link" prefix)
#   get:    bpfman get program|link ID
#   list:   bpfman list programs|links           (no dispatcher list)
#
# WARNING: do not point any other bpfman at /run/bpfman while this
# runs. The Rust CLI's runtime root is hard-coded, and its
# is_bpffs_mounted check does not recognise a bpffs it did not mount
# itself: it mounts a fresh bpffs over the top, shadowing every
# existing pin (observed, not inferred -- stacked mounts in findmnt).
# The Go fixture (program-load.sh) sidesteps this by running against
# its own root, /run/go-bpfman, via BPFMAN_RUNTIME_DIR. Recover from
# shadowing by unloading all Rust programs and unmounting the top
# /run/bpfman/fs layer.
#
# Retprobe caveat: the Rust implementation derives retprobe-ness from
# the object's ELF section (aya ProbeKind), not from the load-type
# alias. The kprobe_counter/uprobe_exact objects carry entry-probe
# sections (kprobe/..., uprobe/...), so the kretprobe/uretprobe loads
# below still produce entry probes under Rust. The rows exist for
# one-to-one comparison with the Go fixture; the Type column will read
# kprobe/uprobe.
#
# Run from the go-bpfman repository root:
#
#   sudo ./program-load-rust.sh
#
# This intentionally does not detach links or unload programs. Clean up
# after inspection by unloading the listed program ids and:
#
#   sudo ip link del bpfmanrust0
#
# Note: the kprobe/kretprobe/fentry/fexit cases target do_unlinkat,
# which is inlined away on some kernels/arches; the script aborts at
# the first such failure on those hosts.

set -euo pipefail

BPFMAN=${BPFMAN:-$HOME/src/github.com/bpfman/bpfman/worktrees/general/target/debug/bpfman}
HOST_LINK=bpfmanrust0
PEER_LINK=bpfmanrust1
TESTDATA=e2e/testdata/bpf

prog_id() { awk '/Program ID:/ {print $NF; exit}'; }
link_id() { awk '/Link ID:/ {print $NF; exit}'; }

# A host veth to attach the xdp/tc/tcx programs to. Both ends are deleted
# by `ip link del bpfmanrust0`.
if ! ip link show "$HOST_LINK" >/dev/null 2>&1; then
	ip link add "$HOST_LINK" type veth peer name "$PEER_LINK"
	ip link set "$HOST_LINK" up
fi

# A stable uprobe target: libc's malloc, resolved from a system binary.
libc=$(ldd "$(command -v cat)" 2>/dev/null | awk '/libc\.so/ {print $3; exit}')
if [[ -z "${libc:-}" ]]; then
	echo "could not resolve libc path for the uprobe target" >&2
	exit 1
fi

# --- load + attach one program of each kind ---

# The xdp program alone carries an application label so the program
# list renders both a populated Application cell and empty ones.
xdp_id=$("$BPFMAN" load file --path "$TESTDATA/xdp_pass.bpf.o" --programs xdp:pass --application demo-app -m fixture=program-load | prog_id)
xdp_link_id=$("$BPFMAN" attach "$xdp_id" xdp --iface "$HOST_LINK" --priority 50 -m fixture=program-load -m kind=xdp | link_id)

tc_id=$("$BPFMAN" load file --path "$TESTDATA/tc_counter.bpf.o" --programs tc:stats -m fixture=program-load | prog_id)
tc_link_id=$("$BPFMAN" attach "$tc_id" tc --direction ingress --iface "$HOST_LINK" --priority 60 --proceed-on ok shot dispatcher_return -m fixture=program-load -m kind=tc | link_id)

tcx_id=$("$BPFMAN" load file --path "$TESTDATA/tcx_counter.bpf.o" --programs tcx:tcx_stats -m fixture=program-load | prog_id)
tcx_link_id=$("$BPFMAN" attach "$tcx_id" tcx --direction ingress --iface "$HOST_LINK" --priority 70 -m fixture=program-load -m kind=tcx | link_id)

tracepoint_id=$("$BPFMAN" load file --path "$TESTDATA/tracepoint_counter.bpf.o" --programs tracepoint:tracepoint_kill_recorder -m fixture=program-load | prog_id)
tracepoint_link_id=$("$BPFMAN" attach "$tracepoint_id" tracepoint --tracepoint syscalls/sys_enter_kill -m fixture=program-load -m kind=tracepoint | link_id)

kprobe_id=$("$BPFMAN" load file --path "$TESTDATA/kprobe_counter.bpf.o" --programs kprobe:kprobe_counter -m fixture=program-load | prog_id)
kprobe_link_id=$("$BPFMAN" attach "$kprobe_id" kprobe --fn-name do_unlinkat -m fixture=program-load -m kind=kprobe | link_id)

kretprobe_id=$("$BPFMAN" load file --path "$TESTDATA/kprobe_counter.bpf.o" --programs kretprobe:kprobe_counter -m fixture=program-load | prog_id)
kretprobe_link_id=$("$BPFMAN" attach "$kretprobe_id" kprobe --fn-name do_unlinkat -m fixture=program-load -m kind=kretprobe | link_id)

uprobe_id=$("$BPFMAN" load file --path "$TESTDATA/uprobe_exact.bpf.o" --programs uprobe:uprobe_counter -m fixture=program-load | prog_id)
uprobe_link_id=$("$BPFMAN" attach "$uprobe_id" uprobe --target "$libc" --fn-name malloc -m fixture=program-load -m kind=uprobe | link_id)

uretprobe_id=$("$BPFMAN" load file --path "$TESTDATA/uprobe_exact.bpf.o" --programs uretprobe:uprobe_counter -m fixture=program-load | prog_id)
uretprobe_link_id=$("$BPFMAN" attach "$uretprobe_id" uprobe --target "$libc" --fn-name malloc -m fixture=program-load -m kind=uretprobe | link_id)

fentry_id=$("$BPFMAN" load file --path "$TESTDATA/fentry_exact.bpf.o" --programs fentry:test_fentry:do_unlinkat -m fixture=program-load | prog_id)
fentry_link_id=$("$BPFMAN" attach "$fentry_id" fentry -m fixture=program-load -m kind=fentry | link_id)

fexit_id=$("$BPFMAN" load file --path "$TESTDATA/fexit_exact.bpf.o" --programs fexit:test_fexit:do_unlinkat -m fixture=program-load | prog_id)
fexit_link_id=$("$BPFMAN" attach "$fexit_id" fexit -m fixture=program-load -m kind=fexit | link_id)

# --- multi-link examples ---
#
# Attach the xdp and tracepoint programs a second time so the lists
# show a program with more than one link: the xdp program twice on the
# same interface (a two-member dispatcher chain) and the tracepoint
# program on a second tracepoint.

xdp_link2_id=$("$BPFMAN" attach "$xdp_id" xdp --iface "$HOST_LINK" --priority 55 --proceed-on pass drop -m fixture=program-load -m kind=xdp | link_id)
tracepoint_link2_id=$("$BPFMAN" attach "$tracepoint_id" tracepoint --tracepoint syscalls/sys_exit_kill -m fixture=program-load -m kind=tracepoint | link_id)

# --- image-loaded examples ---
#
# Load two programs from the published bytecode images (the same ones
# the e2e corpus exercises) so the lists and get views show file- and
# image-sourced programs side by side. Requires network access to
# quay.io on the first run.

image_xdp_id=$("$BPFMAN" load image --image-url quay.io/bpfman-bytecode/go-xdp-counter --programs xdp:xdp_stats -m fixture=program-load | prog_id)
image_xdp_link_id=$("$BPFMAN" attach "$image_xdp_id" xdp --iface "$HOST_LINK" --priority 80 -m fixture=program-load -m kind=xdp-image | link_id)

image_tracepoint_id=$("$BPFMAN" load image --image-url quay.io/bpfman-bytecode/go-tracepoint-counter --programs tracepoint:tracepoint_kill_recorder -m fixture=program-load | prog_id)
image_tracepoint_link_id=$("$BPFMAN" attach "$image_tracepoint_id" tracepoint --tracepoint syscalls/sys_enter_kill -m fixture=program-load -m kind=tracepoint-image | link_id)

# --- map-sharing example ---
#
# Load a second copy of the kprobe counter that borrows the first
# one's maps via --map-owner-id, so the get views show a multi-member
# Maps Used By on both the owner and the borrower, and the borrower a
# populated Map Owner ID.

mapshare_id=$("$BPFMAN" load file --path "$TESTDATA/kprobe_counter.bpf.o" --programs kprobe:kprobe_counter --map-owner-id "$kprobe_id" -m fixture=program-load | prog_id)
mapshare_link_id=$("$BPFMAN" attach "$mapshare_id" kprobe --fn-name do_unlinkat -m fixture=program-load -m kind=kprobe-mapshare | link_id)

# --- flag-variation examples ---
#
# Exercise the less-travelled load and attach flags so the get views
# render every optional field: a uprobe with global-data overrides and
# a PID filter, and an xdp attach through a named network namespace.
# Note the Rust global-arg parser takes bare hex with no 0x prefix.
# The peer end of the veth pair moves into the namespace; clean up
# with:
#
#   sudo ip netns del bpfmanrust-ns
NETNS_NAME=bpfmanrust-ns

if ! ip netns list 2>/dev/null | grep -q "^$NETNS_NAME"; then
	ip netns add "$NETNS_NAME"
	ip link set "$PEER_LINK" netns "$NETNS_NAME"
	ip -n "$NETNS_NAME" link set "$PEER_LINK" up
fi

netns_xdp_link_id=$("$BPFMAN" attach "$xdp_id" xdp --iface "$PEER_LINK" --priority 50 --netns "/var/run/netns/$NETNS_NAME" -m fixture=program-load -m kind=xdp-netns | link_id)

uprobe_flags_id=$("$BPFMAN" load file --path "$TESTDATA/uprobe_exact.bpf.o" --programs uprobe:uprobe_counter -g expected_pid=00000000 -g weight=0100000000000000 -m fixture=program-load | prog_id)
uprobe_flags_link_id=$("$BPFMAN" attach "$uprobe_flags_id" uprobe --target "$libc" --fn-name malloc --pid $$ -m fixture=program-load -m kind=uprobe-flags | link_id)

# --- show the rendered output ---

show() {
	local kind=$1 pid=$2 lid=$3
	echo
	echo "=== $kind ==="
	"$BPFMAN" get program "$pid"
	"$BPFMAN" get link "$lid"
}

show xdp        "$xdp_id"        "$xdp_link_id"
show tc         "$tc_id"         "$tc_link_id"
show tcx        "$tcx_id"        "$tcx_link_id"
show tracepoint "$tracepoint_id" "$tracepoint_link_id"
show kprobe     "$kprobe_id"     "$kprobe_link_id"
show kretprobe  "$kretprobe_id"  "$kretprobe_link_id"
show uprobe     "$uprobe_id"     "$uprobe_link_id"
show uretprobe  "$uretprobe_id"  "$uretprobe_link_id"
show fentry     "$fentry_id"     "$fentry_link_id"
show fexit      "$fexit_id"      "$fexit_link_id"
show xdp-image        "$image_xdp_id"        "$image_xdp_link_id"
show tracepoint-image "$image_tracepoint_id" "$image_tracepoint_link_id"
show kprobe-mapshare  "$mapshare_id"         "$mapshare_link_id"
show xdp-netns        "$xdp_id"              "$netns_xdp_link_id"
show uprobe-flags     "$uprobe_flags_id"     "$uprobe_flags_link_id"

echo
echo "=== program list ==="
"$BPFMAN" list programs

echo
echo "=== link list ==="
"$BPFMAN" list links

echo
echo "=== dispatcher list ==="
echo "(the Rust CLI has no dispatcher list command)"
