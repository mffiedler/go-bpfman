#!/bin/bash
# test-kretprobe-load-attach.sh - Test kretprobe program loading and attachment
#
# This test verifies that kretprobe programs can be loaded and attached.
# Kretprobe shares the kprobe implementation but attaches to function return.
#
# The key difference from kprobe:
# - Load with --programs kretprobe:func_name
# - The link type should show as "kretprobe" (not "kprobe")
#
# Prerequisites:
# - bpfman binary built (bin/bpfman)
# - Root privileges (run with sudo)
# - SQLite3 installed
# - jq installed
# - config/test.toml present

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

# Configuration
BPFMAN="${BPFMAN:-./bin/bpfman}"
CONFIG="${CONFIG:-./config/test.toml}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-kretprobe-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/go-kprobe-counter:latest}"
KPROBE_FN="${KPROBE_FN:-try_to_wake_up}"

# Derived paths
DB_PATH="$RUNTIME_DIR/db/store.db"
BPFFS_ROOT="$RUNTIME_DIR/fs"

# Global state
PROG_ID=""
LINK_ID=""

# Colours
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
log_fail() { echo -e "${RED}[FAIL]${NC} $*"; }

bpfman() {
    "$BPFMAN" --config="$CONFIG" --runtime-dir="$RUNTIME_DIR" "$@"
}

cleanup() {
    log_info "Cleaning up..."
    if [ -n "${LINK_ID:-}" ]; then
        bpfman link detach "$LINK_ID" 2>/dev/null || true
    fi
    if [ -n "${PROG_ID:-}" ]; then
        bpfman program unload "$PROG_ID" 2>/dev/null || true
    fi
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
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

ensure_clean_state() {
    log_info "Ensuring clean initial state..."
    log_info "Using runtime directory: $RUNTIME_DIR"
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}

# Step 1: Load as KRETPROBE (not kprobe)
load_program() {
    log_info "Step 1: Loading program as KRETPROBE type..."
    log_info "Image: $IMAGE"
    log_info "Using: --programs kretprobe:kprobe_counter"

    local output
    output=$(bpfman program load image -o json --programs=kretprobe:kprobe_counter --image-url="$IMAGE" 2>&1)
    PROG_ID=$(echo "$output" | jq -r '.[0].record.program_id')

    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to load program"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"

    # Verify program type is kretprobe (this is the key check!)
    local prog_type
    prog_type=$(echo "$output" | jq -r '.[0].status.kernel.program_type')
    log_info "Program type from kernel: $prog_type"

    # Note: The kernel reports kprobe programs as "kprobe" regardless of kprobe/kretprobe
    # The distinction is in our stored metadata
    # Let's check what's stored in the database
    local stored_type
    stored_type=$(echo "$output" | jq -r '.[0].record.load.program_type')
    log_info "Program type from managed metadata: $stored_type"
    assert_eq "kretprobe" "$stored_type" "Managed program type should be kretprobe"

    log_pass "Kretprobe program loaded successfully"
}

# Step 2: Attach (using kprobe command - retprobe derived from program type)
attach_kretprobe() {
    log_info "Step 2: Attaching kretprobe program to $KPROBE_FN..."
    log_info "Note: Using 'attach <id> kprobe' - retprobe flag derived from program type"

    local output
    output=$(bpfman link attach kprobe --fn-name "$KPROBE_FN" -o json "$PROG_ID" 2>&1)

    LINK_ID=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$LINK_ID" ]; then
        log_fail "Failed to attach kretprobe"
        echo "$output"
        exit 1
    fi
    log_info "Link ID: $LINK_ID"

    # THE KEY CHECK: Link type should be "kretprobe" not "kprobe"
    local link_type
    link_type=$(echo "$output" | jq -r '.record.kind')
    log_info "Link type: $link_type"
    assert_eq "kretprobe" "$link_type" "Link type should be kretprobe (not kprobe)"

    # Verify details
    local fn_name retprobe_flag
    fn_name=$(echo "$output" | jq -r '.record.details.fn_name')
    retprobe_flag=$(echo "$output" | jq -r '.record.details.retprobe')

    assert_eq "$KPROBE_FN" "$fn_name" "Function name should match"
    assert_eq "true" "$retprobe_flag" "Retprobe flag should be true"

    log_pass "Kretprobe attached successfully with correct link type"
}

# Step 3: Verify link in list shows as kretprobe
verify_links() {
    log_info "Step 3: Verifying link type in list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local kretprobe_count
    kretprobe_count=$(echo "$output" | jq '[.links[] | select(.kind == "kretprobe")] | length')
    assert_eq "1" "$kretprobe_count" "Should have 1 kretprobe link"

    # Also verify no kprobe links (to ensure it's not misclassified)
    local kprobe_count
    kprobe_count=$(echo "$output" | jq '[.links[] | select(.kind == "kprobe")] | length')
    assert_eq "0" "$kprobe_count" "Should have 0 kprobe links (it's a kretprobe)"

    log_pass "Link correctly shows as kretprobe type"
}

# Step 4: Check database
verify_database() {
    log_info "Step 4: Verifying database entries..."

    # Check link_registry has correct type
    local link_type
    link_type=$(sqlite3 "$DB_PATH" "SELECT kind FROM links WHERE link_id = $LINK_ID;")
    assert_eq "kretprobe" "$link_type" "Database link_type should be kretprobe"

    # Check kprobe_link_details has retprobe=1
    local retprobe_val
    retprobe_val=$(sqlite3 "$DB_PATH" "SELECT retprobe FROM link_kprobe_details WHERE link_id = $LINK_ID;")
    assert_eq "1" "$retprobe_val" "Database retprobe should be 1"

    log_pass "Database entries correct"
}

# Step 5: Detach and unload
cleanup_test() {
    log_info "Step 5: Detaching and unloading..."

    bpfman link detach "$LINK_ID" 2>&1
    LINK_ID=""
    log_info "Detached"

    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""
    log_info "Unloaded"

    log_pass "Cleanup successful"
}

# Main
main() {
    echo "============================================"
    echo "Kretprobe Program Load/Attach Integration Test"
    echo "============================================"
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_kretprobe
    echo ""

    verify_links
    echo ""

    verify_database
    echo ""

    cleanup_test
    echo ""

    echo "============================================"
    log_pass "All kretprobe tests passed!"
    echo "============================================"
}

main "$@"
