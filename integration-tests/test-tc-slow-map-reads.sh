#!/bin/bash
# test-tc-slow-map-reads.sh - Slow-paced TC test with BPF map reads.
#
# Uses TWO separately-loaded programs (A and B) so each has its own
# tc_stats_map. This lets us observe which position is actually
# counting packets.
#
# Phase 1: Program A at position 0 (only program), traffic flows,
#           A's map grows.
# Phase 2: Program B attached at priority 0 -> position 0,
#           A moves to position 1. Traffic continues.
#           A's map should FREEZE (chain stops at pos 0, TC_ACT_OK
#           not in proceed-on). B's map should GROW.
#
# Prerequisites:
# - bpfman binary built (bin/bpfman)
# - Root privileges (run with sudo)
# - bpftool installed
# - jq installed

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

BPFMAN="${BPFMAN:-./bin/bpfman}"
CONFIG="${CONFIG:-./config/test.toml}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-slow-tc-$$}"
BYTECODE="${BYTECODE:-./e2e/testdata/bpf/tc_counter.bpf.o}"
VETH0="bpf-veth0"
VETH1="bpf-veth1"

bpfman_debug() { "$BPFMAN" --config="$CONFIG" --runtime-dir="$RUNTIME_DIR" --log=debug "$@"; }
bpfman_quiet() { "$BPFMAN" --config="$CONFIG" --runtime-dir="$RUNTIME_DIR" "$@"; }

PROG_A=""
PROG_B=""
LINK_IDS=()
PING_PID=""

cleanup() {
    echo "[cleanup] stopping..."
    [ -n "${PING_PID:-}" ] && kill "$PING_PID" 2>/dev/null || true
    for lid in "${LINK_IDS[@]}"; do
        bpfman_quiet link detach "$lid" 2>/dev/null || true
    done
    [ -n "${PROG_A:-}" ] && bpfman_quiet program unload "$PROG_A" 2>/dev/null || true
    [ -n "${PROG_B:-}" ] && bpfman_quiet program unload "$PROG_B" 2>/dev/null || true
    ip link del "$VETH0" 2>/dev/null || true
    umount "$RUNTIME_DIR/fs" 2>/dev/null || true
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
    rm -f /tmp/bpfman-slow-*.log 2>/dev/null || true
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

# Setup veth pair
ip link del "$VETH0" 2>/dev/null || true
rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
ip link add "$VETH0" type veth peer name "$VETH1"
ip addr add 10.99.0.1/24 dev "$VETH0"
ip addr add 10.99.0.2/24 dev "$VETH1"
ip link set "$VETH0" up
ip link set "$VETH1" up
echo "[setup] veth pair: $VETH0 (10.99.0.1) <-> $VETH1 (10.99.0.2)"

# Load two separate instances of the same bytecode
PROG_A=$(bpfman_quiet program load file -o json \
    --path="$BYTECODE" --programs=tc:stats 2>/dev/null | jq -r '.[0].record.program_id')
PROG_B=$(bpfman_quiet program load file -o json \
    --path="$BYTECODE" --programs=tc:stats 2>/dev/null | jq -r '.[0].record.program_id')
echo "[load] prog_A=$PROG_A (will be the 'original' at priority 55)"
echo "[load] prog_B=$PROG_B (will be the 'copy' at priority 0)"

# Start background traffic on VETH0 -> VETH1
ping -I "$VETH0" -i 0.1 10.99.0.2 >/dev/null 2>&1 &
PING_PID=$!
echo "[traffic] ping -i 0.1 started (pid=$PING_PID)"

# Phase 1: program A at position 0 (only program)
echo ""
echo "=========================================="
echo "PHASE 1: Program A at position 0 (only program)"
echo "=========================================="
LINK_A=$(bpfman_debug link attach tc --iface "$VETH1" --direction ingress --priority 55 -o json "$PROG_A" 2>/tmp/bpfman-slow-a1.log | jq -r '.record.id')
LINK_IDS+=("$LINK_A")
echo "[attach] link_A=$LINK_A priority=55 (position 0, only program)"
grep "TC dispatcher slot config" /tmp/bpfman-slow-a1.log || true

sleep 3
report "PHASE1-3s"
sleep 3
report "PHASE1-6s"

# Phase 2: program B at priority 0 -> position 0, A moves to position 1
echo ""
echo "=========================================="
echo "PHASE 2: Program B at priority=0 -> position 0"
echo "         Program A moves to position 1"
echo "=========================================="
LINK_B=$(bpfman_debug link attach tc --iface "$VETH1" --direction ingress --priority 0 -o json "$PROG_B" 2>/tmp/bpfman-slow-a2.log | jq -r '.record.id')
LINK_IDS+=("$LINK_B")
echo "[attach] link_B=$LINK_B priority=0"
grep "TC dispatcher slot config" /tmp/bpfman-slow-a2.log || true

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
