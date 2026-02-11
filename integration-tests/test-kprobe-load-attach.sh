#!/bin/bash
# test-kprobe-load-attach.sh - Test kprobe program loading and attachment
#
# This test verifies that kprobe programs can be:
# 1. Loaded from an OCI image
# 2. Attached to a kernel function
# 3. Detached cleanly
# 4. Unloaded cleanly
#
# Kprobe is a single-attach program type (no dispatchers).
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
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-kprobe-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/go-kprobe-counter:latest}"
# Kernel function to attach to - try_to_wake_up is commonly available
KPROBE_FN="${KPROBE_FN:-try_to_wake_up}"

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

# Check that kernel function exists
check_kernel_function() {
    log_info "Checking kernel function $KPROBE_FN exists..."

    if [ -f /proc/kallsyms ]; then
        if grep -q " ${KPROBE_FN}$" /proc/kallsyms 2>/dev/null || \
           grep -q " ${KPROBE_FN}\." /proc/kallsyms 2>/dev/null; then
            log_info "Kernel function $KPROBE_FN found"
        else
            log_warn "Kernel function $KPROBE_FN not found in /proc/kallsyms"
            log_warn "Test may fail if function doesn't exist"
        fi
    else
        log_warn "Cannot check /proc/kallsyms - skipping function check"
    fi
}

# Ensure clean initial state
ensure_clean_state() {
    log_info "Ensuring clean initial state..."
    log_info "Using runtime directory: $RUNTIME_DIR"
    log_info "Using config: $CONFIG"
    log_info "Using kernel function: $KPROBE_FN"

    # Clean up any previous run
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}

# Step 1: Load kprobe program from OCI image
load_program() {
    log_info "Step 1: Loading kprobe program from OCI image..."
    log_info "Image: $IMAGE"

    local output
    output=$(bpfman program load image -o json --programs=kprobe:kprobe_counter --image-url="$IMAGE" 2>&1)
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
    assert_eq "kprobe" "$prog_type" "Managed program type should be kprobe"

    local kernel_type
    kernel_type=$(echo "$output" | jq -r '.[0].status.kernel.program_type')
    assert_eq "kprobe" "$kernel_type" "Kernel program type should be kprobe"

    local prog_name
    prog_name=$(echo "$output" | jq -r '.[0].status.kernel.name')
    assert_eq "kprobe_counter" "$prog_name" "Program name should be kprobe_counter"

    log_pass "Kprobe program loaded successfully"
}

# Step 2: Attach to kernel function
attach_kprobe() {
    log_info "Step 2: Attaching kprobe program to $KPROBE_FN..."

    local output
    output=$(bpfman link attach kprobe --fn-name "$KPROBE_FN" -o json "$PROG_ID" 2>&1)

    LINK_ID=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$LINK_ID" ]; then
        log_fail "Failed to attach kprobe"
        echo "$output"
        exit 1
    fi
    log_info "Link ID: $LINK_ID"

    # Verify link details
    local link_type fn_name
    link_type=$(echo "$output" | jq -r '.record.kind')
    fn_name=$(echo "$output" | jq -r '.record.details.fn_name')

    assert_eq "kprobe" "$link_type" "Link type should be kprobe"
    assert_eq "$KPROBE_FN" "$fn_name" "Function name should be $KPROBE_FN"

    log_pass "Kprobe program attached to $KPROBE_FN"
}

# Step 3: Verify link in list
verify_links() {
    log_info "Step 3: Verifying link via list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local kprobe_link_count
    kprobe_link_count=$(echo "$output" | jq '[.links[] | select(.kind == "kprobe")] | length')

    assert_eq "1" "$kprobe_link_count" "Should have 1 kprobe link"

    log_pass "Link verified"
}

# Step 4: Verify no dispatchers (kprobe is single-attach)
verify_no_dispatchers() {
    log_info "Step 4: Verifying no dispatchers (kprobe is single-attach)..."

    # Check database for dispatchers - should be none for kprobe
    local disp_count
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers;" 2>/dev/null || echo "0")

    assert_eq "0" "$disp_count" "Should have 0 dispatchers (kprobe is single-attach)"

    log_pass "No dispatchers verified (as expected for kprobe)"
}

# Step 5: Detach link
detach_link() {
    log_info "Step 5: Detaching kprobe link..."

    bpfman link detach "$LINK_ID" 2>&1
    local saved_link_id="$LINK_ID"
    LINK_ID=""  # Clear so cleanup doesn't try again

    # Verify link is gone
    local link_output kprobe_link_count
    link_output=$(bpfman link list -o json 2>&1)
    kprobe_link_count=$(echo "$link_output" | jq '[.links[] | select(.kind == "kprobe")] | length')
    assert_eq "0" "$kprobe_link_count" "Should have 0 kprobe links after detach"

    log_pass "Kprobe link detached"
}

# Step 6: Unload program
unload_program() {
    log_info "Step 6: Unloading kprobe program..."
    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""  # Clear so cleanup doesn't try again
    log_pass "Kprobe program unloaded"
}

# Step 7: Final verification
verify_final_state() {
    log_info "Step 7: Final verification..."

    # Check database - all zeros
    local prog_count kprobe_link_count
    prog_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM managed_programs;")
    kprobe_link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_kprobe_details;")

    assert_eq "0" "$prog_count" "Should have 0 programs"
    assert_eq "0" "$kprobe_link_count" "Should have 0 kprobe link details"

    log_pass "Final state verified - clean"
}

# Main
main() {
    echo "=========================================="
    echo "Kprobe Program Load/Attach Integration Test"
    echo "=========================================="
    echo ""

    check_kernel_function
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_kprobe
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
    log_pass "All kprobe tests passed!"
    echo "=========================================="
}

main "$@"
