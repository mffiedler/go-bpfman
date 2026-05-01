#!/bin/bash
# test-tracepoint-load-attach.sh - Test tracepoint program loading and attachment
#
# This test verifies that tracepoint programs can be:
# 1. Loaded from an OCI image
# 2. Attached to a kernel tracepoint
# 3. Detached cleanly
# 4. Unloaded cleanly
#
# Tracepoint is a single-attach program type (no dispatchers).
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
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-tracepoint-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/go-tracepoint-counter:latest}"
# Tracepoint to attach to - syscalls/sys_enter_kill is commonly available
TRACEPOINT="${TRACEPOINT:-syscalls/sys_enter_kill}"

# Derived paths (matching RuntimeDirs structure)
DB_PATH="$RUNTIME_DIR/db/store.db"

# Global state
PROG_ID=""
LINK_ID=""
BPFFS_ROOT="$RUNTIME_DIR/fs"

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
    if [ -n "${LINK_ID:-}" ]; then
        bpfman link detach "$LINK_ID" 2>/dev/null || true
    fi
    # Unload any test programs
    if [ -n "${PROG_ID:-}" ]; then
        bpfman program unload "$PROG_ID" 2>/dev/null || true
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

# Check that tracepoint exists
check_tracepoint() {
    log_info "Checking tracepoint $TRACEPOINT exists..."

    # Parse tracepoint into group/name
    local group name
    group=$(echo "$TRACEPOINT" | cut -d/ -f1)
    name=$(echo "$TRACEPOINT" | cut -d/ -f2)

    local tracepoint_path="/sys/kernel/debug/tracing/events/$group/$name"
    if test -d "$tracepoint_path"; then
        log_info "Tracepoint $TRACEPOINT found at $tracepoint_path"
    else
        log_warn "Tracepoint $TRACEPOINT not found at $tracepoint_path"
        log_warn "Test may fail if tracepoint doesn't exist"
    fi
}

# Ensure clean initial state
ensure_clean_state() {
    log_info "Ensuring clean initial state..."
    log_info "Using runtime directory: $RUNTIME_DIR"
    log_info "Using config: $CONFIG"
    log_info "Using tracepoint: $TRACEPOINT"

    # Clean up any previous run
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}

# Step 1: Load tracepoint program from OCI image
load_program() {
    log_info "Step 1: Loading tracepoint program from OCI image..."
    log_info "Image: $IMAGE"

    local output
    output=$(bpfman program load image -o json --programs=tracepoint:tracepoint_kill_recorder --image-url="$IMAGE" 2>&1)
    PROG_ID=$(echo "$output" | jq -r '.programs[0].record.program_id')

    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to load program"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"

    # Verify program info
    local prog_type
    prog_type=$(echo "$output" | jq -r '.programs[0].record.load.program_type')
    assert_eq "tracepoint" "$prog_type" "Managed program type should be tracepoint"

    local kernel_type
    kernel_type=$(echo "$output" | jq -r '.[0].status.kernel.program_type')
    assert_eq "tracepoint" "$kernel_type" "Kernel program type should be tracepoint"

    local prog_name
    prog_name=$(echo "$output" | jq -r '.[0].status.kernel.name')
    # Kernel truncates long names - tracepoint_kill_recorder becomes tracepoint_kill
    assert_eq "tracepoint_kill" "$prog_name" "Program name should be tracepoint_kill"

    log_pass "Tracepoint program loaded successfully"
}

# Step 2: Attach to tracepoint
attach_tracepoint() {
    log_info "Step 2: Attaching tracepoint program to $TRACEPOINT..."

    local output
    output=$(bpfman link attach tracepoint --tracepoint "$TRACEPOINT" -o json "$PROG_ID" 2>&1)

    LINK_ID=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$LINK_ID" ]; then
        log_fail "Failed to attach tracepoint"
        echo "$output"
        exit 1
    fi
    log_info "Link ID: $LINK_ID"

    # Verify link details
    local link_type tp_group tp_name
    link_type=$(echo "$output" | jq -r '.record.kind')
    tp_group=$(echo "$output" | jq -r '.record.details.group')
    tp_name=$(echo "$output" | jq -r '.record.details.name')

    assert_eq "tracepoint" "$link_type" "Link type should be tracepoint"
    assert_eq "$TRACEPOINT" "$tp_group/$tp_name" "Tracepoint should be $TRACEPOINT"

    log_pass "Tracepoint program attached to $TRACEPOINT"
}

# Step 3: Verify link in list
verify_links() {
    log_info "Step 3: Verifying link via list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local tracepoint_link_count
    tracepoint_link_count=$(echo "$output" | jq '[.links[] | select(.kind == "tracepoint")] | length')

    assert_eq "1" "$tracepoint_link_count" "Should have 1 tracepoint link"

    log_pass "Link verified"
}

# Step 4: Verify no dispatchers (tracepoint is single-attach)
verify_no_dispatchers() {
    log_info "Step 4: Verifying no dispatchers (tracepoint is single-attach)..."

    # Check database for dispatchers - should be none for tracepoint
    local disp_count
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers;" 2>/dev/null || echo "0")

    assert_eq "0" "$disp_count" "Should have 0 dispatchers (tracepoint is single-attach)"

    log_pass "No dispatchers verified (as expected for tracepoint)"
}

# Step 5: Detach link
detach_link() {
    log_info "Step 5: Detaching tracepoint link..."

    bpfman link detach "$LINK_ID" 2>&1
    local saved_link_id="$LINK_ID"
    LINK_ID=""  # Clear so cleanup doesn't try again

    # Verify link is gone
    local link_output tracepoint_link_count
    link_output=$(bpfman link list -o json 2>&1)
    tracepoint_link_count=$(echo "$link_output" | jq '[.links[] | select(.kind == "tracepoint")] | length')
    assert_eq "0" "$tracepoint_link_count" "Should have 0 tracepoint links after detach"

    log_pass "Tracepoint link detached"
}

# Step 6: Unload program
unload_program() {
    log_info "Step 6: Unloading tracepoint program..."
    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""  # Clear so cleanup doesn't try again
    log_pass "Tracepoint program unloaded"
}

# Step 7: Final verification
verify_final_state() {
    log_info "Step 7: Final verification..."

    # Check database - all zeros
    local prog_count tracepoint_link_count
    prog_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM managed_programs;")
    tracepoint_link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_tracepoint_details;")

    assert_eq "0" "$prog_count" "Should have 0 programs"
    assert_eq "0" "$tracepoint_link_count" "Should have 0 tracepoint link details"

    log_pass "Final state verified - clean"
}

# Main
main() {
    echo "=========================================="
    echo "Tracepoint Program Load/Attach Integration Test"
    echo "=========================================="
    echo ""

    check_tracepoint
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_tracepoint
    echo ""

    verify_links
    echo ""

    verify_no_dispatchers
    echo ""

    detach_link
    echo ""

    unload_program
    echo ""

    verify_final_state
    echo ""

    echo "=========================================="
    log_pass "All tracepoint tests passed!"
    echo "=========================================="
}

main "$@"
