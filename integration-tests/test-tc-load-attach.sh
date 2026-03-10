#!/bin/bash
# test-tc-load-attach.sh - Test TC program loading and attachment
#
# This test verifies that TC programs can be:
# 1. Loaded from an OCI image
# 2. Attached to a network interface (ingress/egress)
# 3. Detached cleanly
# 4. Unloaded cleanly
#
# The test uses both ingress and egress directions to verify full coverage.
#
# Prerequisites:
# - bpfman binary built (bin/bpfman)
# - Root privileges (run with sudo)
# - SQLite3 installed
# - jq installed
# - config/test.toml present (with signature verification disabled)

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

# Configuration - can be overridden via environment
BPFMAN="${BPFMAN:-./bin/bpfman}"
CONFIG="${CONFIG:-./config/test.toml}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-tc-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/go-tc-counter:latest}"
IFACE="bpfman-tc"

# Derived paths (matching RuntimeDirs structure)
DB_PATH="$RUNTIME_DIR/db/store.db"

# Global state
PROG_ID=""
LINK_IDS=()
BPFFS_ROOT="$RUNTIME_DIR/fs"
BPFFS_TC_INGRESS="$BPFFS_ROOT/tc-ingress"
BPFFS_TC_EGRESS="$BPFFS_ROOT/tc-egress"

# Colours for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No colour

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
log_fail() { echo -e "${RED}[FAIL]${NC} $*"; }

bpfman() {
    "$BPFMAN" --config="$CONFIG" --runtime-dir="$RUNTIME_DIR" "$@"
}

cleanup() {
    log_info "Cleaning up..."
    # Detach any test links
    if [ "${#LINK_IDS[@]}" -gt 0 ] 2>/dev/null; then
        for link_id in "${LINK_IDS[@]}"; do
            bpfman link detach "$link_id" 2>/dev/null || true
        done
    fi
    # Unload any test programs
    if [ -n "${PROG_ID:-}" ]; then
        bpfman program unload "$PROG_ID" 2>/dev/null || true
    fi
    # Delete test interface
    if ip link show "$IFACE" &>/dev/null; then
        ip link del "$IFACE" 2>/dev/null || true
    fi
    # Unmount bpffs if mounted
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    # Remove runtime directory
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}
trap cleanup EXIT

assert_eq() {
    local expected="$1"
    local actual="$2"
    local msg="${3:-assertion failed}"
    if [ "$expected" != "$actual" ]; then
        log_fail "$msg: expected '$expected', got '$actual'"
        exit 1
    fi
}

assert_ne() {
    local unexpected="$1"
    local actual="$2"
    local msg="${3:-assertion failed}"
    if [ "$unexpected" = "$actual" ]; then
        log_fail "$msg: got unexpected value '$actual'"
        exit 1
    fi
}

# Create a test dummy interface. If it already exists (leaked from a
# previous run), delete it first.
create_test_interface() {
    if ip link show "$IFACE" &>/dev/null; then
        log_warn "Interface $IFACE already exists (leaked from previous run?), removing"
        ip link del "$IFACE" 2>/dev/null || true
    fi
    ip link add "$IFACE" type dummy
    ip link set "$IFACE" up
    log_info "Created test interface: $IFACE"
}

# Ensure clean initial state
ensure_clean_state() {
    log_info "Ensuring clean initial state..."
    log_info "Using runtime directory: $RUNTIME_DIR"
    log_info "Using config: $CONFIG"
    log_info "Using interface: $IFACE"

    # Clean up any previous run
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true

    create_test_interface
}

# Step 1: Load TC program from OCI image
load_program() {
    log_info "Step 1: Loading TC program from OCI image..."
    log_info "Image: $IMAGE"

    local output
    output=$(bpfman program load image -o json --programs=tc:stats --image-url="$IMAGE" 2>&1)
    PROG_ID=$(echo "$output" | jq -r '.[0].record.program_id')

    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to load program"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"

    # Verify program info
    local prog_type
    prog_type=$(echo "$output" | jq -r '.[0].record.load.program_type')
    assert_eq "tc" "$prog_type" "Managed program type should be tc"

    local kernel_type
    kernel_type=$(echo "$output" | jq -r '.[0].status.kernel.program_type')
    assert_eq "extension" "$kernel_type" "Kernel program type should be extension"

    local prog_name
    prog_name=$(echo "$output" | jq -r '.[0].status.kernel.name')
    assert_eq "stats" "$prog_name" "Program name should be stats"

    log_pass "TC program loaded successfully"
}

# Step 2: Attach to ingress
attach_ingress() {
    log_info "Step 2: Attaching TC program to $IFACE ingress..."
    LINK_IDS=()

    local output
    output=$(bpfman link attach tc --iface "$IFACE" --direction ingress --priority 50 -o json "$PROG_ID" 2>&1)

    local link_id
    link_id=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$link_id" ]; then
        log_fail "Failed to attach to ingress"
        echo "$output"
        exit 1
    fi
    LINK_IDS+=("$link_id")
    log_info "Ingress link ID: $link_id"

    # Verify link details
    local link_type direction iface
    link_type=$(echo "$output" | jq -r '.record.kind')
    direction=$(echo "$output" | jq -r '.record.details.direction')
    iface=$(echo "$output" | jq -r '.record.details.interface')

    assert_eq "tc" "$link_type" "Link type should be tc"
    assert_eq "ingress" "$direction" "Direction should be ingress"
    assert_eq "$IFACE" "$iface" "Interface should be $IFACE"

    log_pass "TC program attached to ingress"
}

# Step 3: Attach to egress
attach_egress() {
    log_info "Step 3: Attaching TC program to $IFACE egress..."

    local output
    output=$(bpfman link attach tc --iface "$IFACE" --direction egress --priority 50 -o json "$PROG_ID" 2>&1)

    local link_id
    link_id=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$link_id" ]; then
        log_fail "Failed to attach to egress"
        echo "$output"
        exit 1
    fi
    LINK_IDS+=("$link_id")
    log_info "Egress link ID: $link_id"

    # Verify link details
    local link_type direction
    link_type=$(echo "$output" | jq -r '.record.kind')
    direction=$(echo "$output" | jq -r '.record.details.direction')

    assert_eq "tc" "$link_type" "Link type should be tc"
    assert_eq "egress" "$direction" "Direction should be egress"

    log_pass "TC program attached to egress"
}

# Step 4: Verify dispatchers exist
verify_dispatchers() {
    log_info "Step 4: Verifying TC dispatchers..."

    # Check database for dispatchers
    local ingress_disp egress_disp
    ingress_disp=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers WHERE type='tc-ingress';")
    egress_disp=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers WHERE type='tc-egress';")

    assert_eq "1" "$ingress_disp" "Should have 1 tc-ingress dispatcher"
    assert_eq "1" "$egress_disp" "Should have 1 tc-egress dispatcher"

    # Check filesystem
    if [ ! -d "$BPFFS_TC_INGRESS" ]; then
        log_fail "TC ingress directory does not exist: $BPFFS_TC_INGRESS"
        exit 1
    fi
    if [ ! -d "$BPFFS_TC_EGRESS" ]; then
        log_fail "TC egress directory does not exist: $BPFFS_TC_EGRESS"
        exit 1
    fi

    # Check for dispatcher links
    local ingress_link egress_link
    ingress_link=$(ls "$BPFFS_TC_INGRESS" 2>/dev/null | grep -c "dispatcher_" || echo "0")
    egress_link=$(ls "$BPFFS_TC_EGRESS" 2>/dev/null | grep -c "dispatcher_" || echo "0")

    assert_ne "0" "$ingress_link" "TC ingress dispatcher should exist"
    assert_ne "0" "$egress_link" "TC egress dispatcher should exist"

    log_pass "TC dispatchers verified"
}

# Step 5: List links and verify
verify_links() {
    log_info "Step 5: Verifying links via list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local tc_link_count
    tc_link_count=$(echo "$output" | jq '[.links[] | select(.kind == "tc")] | length')

    assert_eq "2" "$tc_link_count" "Should have 2 TC links (ingress + egress)"

    log_pass "Links verified"
}

# Step 6: Detach all links
detach_all() {
    log_info "Step 6: Detaching all TC links..."

    for link_id in "${LINK_IDS[@]}"; do
        log_info "Detaching link $link_id..."
        bpfman link detach "$link_id" 2>&1
    done
    LINK_IDS=()

    # Verify links are gone
    local link_output tc_link_count
    link_output=$(bpfman link list -o json 2>&1)
    tc_link_count=$(echo "$link_output" | jq '[.links[] | select(.kind == "tc")] | length')
    assert_eq "0" "$tc_link_count" "Should have 0 TC links after detach"

    # Verify dispatchers are cleaned up
    local disp_count
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers WHERE type LIKE 'tc-%';")
    assert_eq "0" "$disp_count" "Should have 0 TC dispatchers after all detaches"

    log_pass "All TC links detached and dispatchers cleaned up"
}

# Step 7: Unload program
unload_program() {
    log_info "Step 7: Unloading TC program..."
    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""  # Clear so cleanup doesn't try again
    log_pass "TC program unloaded"
}

# Step 8: Final verification
verify_final_state() {
    log_info "Step 8: Final verification..."

    # Check database - all zeros for TC
    local prog_count tc_link_count disp_count
    prog_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM managed_programs;")
    tc_link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_tc_details;")
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers WHERE type LIKE 'tc-%';")

    assert_eq "0" "$prog_count" "Should have 0 programs"
    assert_eq "0" "$tc_link_count" "Should have 0 TC link details"
    assert_eq "0" "$disp_count" "Should have 0 TC dispatchers"

    log_pass "Final state verified - clean"
}

# Main
main() {
    echo "========================================"
    echo "TC Program Load/Attach Integration Test"
    echo "========================================"
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_ingress
    echo ""

    attach_egress
    echo ""

    verify_dispatchers
    echo ""

    verify_links
    echo ""

    detach_all
    echo ""

    unload_program
    echo ""

    verify_final_state
    echo ""

    echo "========================================"
    log_pass "All TC tests passed!"
    echo "========================================"
}

main "$@"
