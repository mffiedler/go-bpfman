#!/bin/bash
# test-tc-multi-priority.sh - Reproduce operator TestTcGoCounterLinkPriority
#
# This test replicates the operator integration test by:
# 1. Loading a TC program
# 2. Attaching it 5 times with different priorities (0, 50, 55, 500, 1000)
# 3. After each attach, verifying all existing links via `bpfman link get`
# 4. Verifying final link ordering (positions sorted by priority)
#
# Prerequisites:
# - bpfman binary built (bin/bpfman)
# - Root privileges (run with sudo)
# - e2e/testdata/bpf/tc_counter.bpf.o present
# - jq installed

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

BPFMAN="${BPFMAN:-./bin/bpfman}"
CONFIG="${CONFIG:-./config/test.toml}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-tc-multi-priority-$$}"
BYTECODE="${BYTECODE:-./e2e/testdata/bpf/tc_counter.bpf.o}"
IFACE="bpfman-tcp"

DB_PATH="$RUNTIME_DIR/db/store.db"

# Colours
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_pass()  { echo -e "${GREEN}[PASS]${NC} $*"; }
log_fail()  { echo -e "${RED}[FAIL]${NC} $*"; }

bpfman() {
    "$BPFMAN" --config="$CONFIG" --runtime-dir="$RUNTIME_DIR" "$@"
}

PROG_ID=""
LINK_IDS=()
BPFFS_ROOT="$RUNTIME_DIR/fs"

cleanup() {
    log_info "Cleaning up..."
    if [ "${#LINK_IDS[@]}" -gt 0 ] 2>/dev/null; then
        for link_id in "${LINK_IDS[@]}"; do
            bpfman link detach "$link_id" 2>/dev/null || true
        done
    fi
    if [ -n "${PROG_ID:-}" ]; then
        bpfman program unload "$PROG_ID" 2>/dev/null || true
    fi
    if ip link show "$IFACE" &>/dev/null; then
        ip link del "$IFACE" 2>/dev/null || true
    fi
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}
trap cleanup EXIT

assert_eq() {
    local expected="$1" actual="$2" msg="${3:-assertion failed}"
    if [ "$expected" != "$actual" ]; then
        log_fail "$msg: expected '$expected', got '$actual'"
        exit 1
    fi
}

assert_ne() {
    local unexpected="$1" actual="$2" msg="${3:-assertion failed}"
    if [ "$unexpected" = "$actual" ]; then
        log_fail "$msg: got unexpected value '$actual'"
        exit 1
    fi
}

create_test_interface() {
    if ip link show "$IFACE" &>/dev/null; then
        log_warn "Interface $IFACE already exists, removing"
        ip link del "$IFACE" 2>/dev/null || true
    fi
    ip link add "$IFACE" type dummy
    ip link set "$IFACE" up
    log_info "Created test interface: $IFACE"
}

ensure_clean_state() {
    log_info "Runtime directory: $RUNTIME_DIR"
    log_info "Bytecode: $BYTECODE"
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
    create_test_interface
}

load_program() {
    log_info "Loading TC program from $BYTECODE..."
    local output
    output=$(bpfman program load file -o json \
        --path="$BYTECODE" \
        --programs=tc:stats \
        2>&1)
    PROG_ID=$(echo "$output" | jq -r '.[0].record.program_id')
    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to load program"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"
    log_pass "TC program loaded"
}

# attach_tc <priority> [--no-priority]
# Attaches the TC program and appends the link ID to LINK_IDS.
# If --no-priority is passed, priority is omitted (defaults to 0).
attach_tc() {
    local priority_flag=""
    if [ "${1:-}" = "--no-priority" ]; then
        log_info "Attaching TC program (no explicit priority)..."
    else
        local priority="$1"
        priority_flag="--priority $priority"
        log_info "Attaching TC program with priority=$priority..."
    fi

    local output
    # shellcheck disable=SC2086
    output=$(bpfman link attach tc \
        --iface "$IFACE" \
        --direction ingress \
        $priority_flag \
        -o json \
        "$PROG_ID" 2>&1)

    local link_id
    link_id=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true
    if [ -z "$link_id" ]; then
        log_fail "Failed to attach"
        echo "$output"
        exit 1
    fi
    LINK_IDS+=("$link_id")
    log_info "  Link ID: $link_id"

    # Verify the link is retrievable
    local get_output
    get_output=$(bpfman link get -o json "$link_id" 2>&1)
    local get_id
    get_id=$(echo "$get_output" | jq -r '.record.id // empty' 2>/dev/null) || true
    assert_eq "$link_id" "$get_id" "link get should return the link we just created"
    log_pass "  Attached and verified link $link_id"
}

# verify_all_links checks that every link in LINK_IDS is retrievable.
# Output is sorted by position so the display reads naturally.
verify_all_links() {
    log_info "Verifying all ${#LINK_IDS[@]} links are retrievable..."
    local failures=0
    local entries=()
    for link_id in "${LINK_IDS[@]}"; do
        local output
        output=$(bpfman link get -o json "$link_id" 2>&1)
        local got_id
        got_id=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true
        if [ "$got_id" != "$link_id" ]; then
            log_fail "  Link $link_id: NOT FOUND"
            echo "  Output: $output"
            failures=$((failures + 1))
        else
            local priority position
            priority=$(echo "$output" | jq -r '.record.details.priority // "?"')
            position=$(echo "$output" | jq -r '.record.details.position // "?"')
            entries+=("$position $link_id $priority")
        fi
    done
    # Print sorted by position
    printf '%s\n' "${entries[@]}" | sort -n | while read -r pos lid pri; do
        log_info "  Link $lid: priority=$pri position=$pos"
    done
    if [ "$failures" -gt 0 ]; then
        log_fail "$failures link(s) not found!"
        exit 1
    fi
    log_pass "All links verified"
}

# verify_link_order checks that positions are assigned in
# non-decreasing priority order.
verify_link_order() {
    log_info "Verifying link ordering..."
    local all_links
    all_links=$(bpfman link list -o json 2>&1)

    # Collect TC links with their positions and priorities
    local tc_links
    tc_links=$(echo "$all_links" | jq -c '[.links[] | select(.kind == "tc")] | sort_by(.details.position)')

    local count
    count=$(echo "$tc_links" | jq 'length')
    assert_eq "${#LINK_IDS[@]}" "$count" "TC link count"

    local prev_priority=-1
    for i in $(seq 0 $((count - 1))); do
        local priority position link_id
        priority=$(echo "$tc_links" | jq -r ".[$i].details.priority")
        position=$(echo "$tc_links" | jq -r ".[$i].details.position")
        link_id=$(echo "$tc_links" | jq -r ".[$i].id")

        # For priority=0 (unspecified), effective priority is 50
        local effective_priority=$priority
        if [ "$effective_priority" -eq 0 ]; then
            effective_priority=50
        fi

        log_info "  Position $position: link=$link_id priority=$priority (effective=$effective_priority)"

        if [ "$effective_priority" -lt "$prev_priority" ]; then
            log_fail "Priority ordering violated at position $position: $effective_priority < $prev_priority"
            exit 1
        fi
        prev_priority=$effective_priority
    done
    log_pass "Link ordering verified (priorities non-decreasing)"
}

main() {
    echo "============================================="
    echo "TC Multi-Priority Test (Operator Equivalent)"
    echo "============================================="
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    # Replicate the operator test priorities:
    # The operator creates 5 TcPrograms with priorities:
    #   nil (unspecified) -> defaults to 0
    #   0
    #   500
    #   1000
    #   55 (from kustomize overlay)
    #
    # After each attach, verify ALL existing links are still
    # retrievable. This catches the re-insertion bug where
    # earlier links' details are lost during rebuild.

    log_info "=== Attach 1: no explicit priority (defaults to 0) ==="
    attach_tc --no-priority
    verify_all_links
    echo ""

    log_info "=== Attach 2: priority=0 ==="
    attach_tc 0
    verify_all_links
    echo ""

    log_info "=== Attach 3: priority=500 ==="
    attach_tc 500
    verify_all_links
    echo ""

    log_info "=== Attach 4: priority=1000 ==="
    attach_tc 1000
    verify_all_links
    echo ""

    log_info "=== Attach 5: priority=55 ==="
    attach_tc 55
    verify_all_links
    echo ""

    # Final verification
    verify_link_order
    echo ""

    echo "============================================="
    log_pass "All TC multi-priority tests passed!"
    echo "============================================="
}

main "$@"
