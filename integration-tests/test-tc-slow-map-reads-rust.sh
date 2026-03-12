#!/bin/bash
# test-tc-slow-map-reads-rust.sh - Same test as test-tc-slow-map-reads.sh
# but using the Rust bpfman binary for comparison.
#
# Uses TWO separately-loaded programs (A and B) so each has its own
# tc_stats_map. Proves that the chain stops at position 0 when
# TC_ACT_OK is not in proceed-on.
#
# Prerequisites:
# - Rust bpfman binary (set RUST_BPFMAN env var or use default path)
# - Root privileges (run with sudo)
# - bpftool installed

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

RUST_BPFMAN="${RUST_BPFMAN:-/home/aim/src/github.com/bpfman/bpfman/worktrees/general/target/debug/bpfman}"
BYTECODE="${BYTECODE:-./e2e/testdata/tc_counter.bpf.o}"
VETH0="bpf-veth0"
VETH1="bpf-veth1"

bpfman() { "$RUST_BPFMAN" "$@"; }

PROG_A=""
PROG_B=""
LINK_A=""
LINK_B=""
PING_PID=""

cleanup() {
    echo "[cleanup] stopping..."
    [ -n "${PING_PID:-}" ] && kill "$PING_PID" 2>/dev/null || true
    [ -n "${LINK_B:-}" ] && bpfman detach "$LINK_B" 2>/dev/null || true
    [ -n "${LINK_A:-}" ] && bpfman detach "$LINK_A" 2>/dev/null || true
    [ -n "${PROG_B:-}" ] && bpfman unload "$PROG_B" 2>/dev/null || true
    [ -n "${PROG_A:-}" ] && bpfman unload "$PROG_A" 2>/dev/null || true
    ip link del "$VETH0" 2>/dev/null || true
}
trap cleanup EXIT

# sum_rx_packets <prog_id> - sum rx_packets across all CPUs
sum_rx_packets() {
    local prog_id="$1"
    local map_ids
    map_ids=$(bpftool prog show id "$prog_id" 2>/dev/null | grep -oP 'map_ids \K[0-9,]+' || true)
    if [ -z "$map_ids" ]; then
        echo "0"
        return
    fi
    for mid in ${map_ids//,/ }; do
        local name
        name=$(bpftool map show id "$mid" 2>/dev/null | grep -oP 'name \K\S+' || echo "?")
        if [[ "$name" == *stats* ]]; then
            local total
            total=$(bpftool map dump id "$mid" 2>/dev/null \
                | grep -oP '"rx_packets": \K[0-9]+' \
                | paste -sd+ | bc)
            echo "${total:-0}"
            return
        fi
    done
    echo "0"
}

report() {
    local label="$1"
    local pkts_a pkts_b
    pkts_a=$(sum_rx_packets "$PROG_A")
    pkts_b=$(sum_rx_packets "$PROG_B")
    echo "[$label] prog_A(id=$PROG_A) rx_packets=$pkts_a  prog_B(id=$PROG_B) rx_packets=$pkts_b"
}

# extract_field <field> from Rust bpfman table output
extract_field() {
    local field="$1"
    grep -oP "$field:\s+\K\S+" | head -1
}

# Setup veth pair
ip link del "$VETH0" 2>/dev/null || true
ip link add "$VETH0" type veth peer name "$VETH1"
ip addr add 10.99.0.1/24 dev "$VETH0" 2>/dev/null || true
ip addr add 10.99.0.2/24 dev "$VETH1" 2>/dev/null || true
ip link set "$VETH0" up
ip link set "$VETH1" up
echo "[setup] veth pair: $VETH0 (10.99.0.1) <-> $VETH1 (10.99.0.2)"
echo "[setup] using Rust bpfman: $RUST_BPFMAN"

# Load two separate instances
load_output_a=$(bpfman load file --programs tc:stats --path "$BYTECODE" 2>&1)
PROG_A=$(echo "$load_output_a" | extract_field "Program ID")
load_output_b=$(bpfman load file --programs tc:stats --path "$BYTECODE" 2>&1)
PROG_B=$(echo "$load_output_b" | extract_field "Program ID")
echo "[load] prog_A=$PROG_A (original, priority 55)"
echo "[load] prog_B=$PROG_B (copy, priority 1)"

# Start background traffic
ping -I "$VETH0" -i 0.1 10.99.0.2 >/dev/null 2>&1 &
PING_PID=$!
echo "[traffic] ping -i 0.1 started (pid=$PING_PID)"

# Phase 1: program A at position 0 (only program)
echo ""
echo "=========================================="
echo "PHASE 1: Program A at position 0 (only program)"
echo "=========================================="
attach_output_a=$(bpfman attach "$PROG_A" tc --direction ingress --iface "$VETH1" --priority 55 2>&1)
LINK_A=$(echo "$attach_output_a" | extract_field "Link ID")
echo "[attach] link_A=$LINK_A priority=55"
echo "$attach_output_a" | grep -E "Position|Proceed On" || true

sleep 3
report "PHASE1-3s"
sleep 3
report "PHASE1-6s"

# Phase 2: program B at priority 1 -> position 0, A moves to position 1
echo ""
echo "=========================================="
echo "PHASE 2: Program B at priority=1 -> position 0"
echo "         Program A moves to position 1"
echo "=========================================="
attach_output_b=$(bpfman attach "$PROG_B" tc --direction ingress --iface "$VETH1" --priority 1 2>&1)
LINK_B=$(echo "$attach_output_b" | extract_field "Link ID")
echo "[attach] link_B=$LINK_B priority=1"
echo "$attach_output_b" | grep -E "Position|Proceed On" || true

sleep 3
report "PHASE2-3s"
sleep 3
report "PHASE2-6s"
sleep 3
report "PHASE2-9s"

echo ""
echo "=========================================="
echo "EXPECTED: A's count freezes after PHASE 2 attach."
echo "          B's count grows (it is at position 0)."
echo "=========================================="
