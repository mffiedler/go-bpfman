#!/bin/bash
# test-uprobe-load-attach.sh - Test uprobe program loading and attachment
#
# This test verifies that uprobe programs can be:
# 1. Loaded from an OCI image
# 2. Attached to a user-space function in a target binary/library
# 3. Detached cleanly
# 4. Unloaded cleanly
#
# Uprobe is a single-attach program type (no dispatchers).
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
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-uprobe-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/go-uprobe-counter:latest}"
# Target library and function - malloc in libc is commonly available
UPROBE_TARGET="${UPROBE_TARGET:-/lib64/libc.so.6}"
UPROBE_FN="${UPROBE_FN:-malloc}"

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

# Check that target library exists
check_target_library() {
    log_info "Checking target library $UPROBE_TARGET exists..."

    if [ -f "$UPROBE_TARGET" ]; then
        log_info "Target library $UPROBE_TARGET found"
        return
    fi

    # Try alternative paths for standard distros
    for alt in /lib/x86_64-linux-gnu/libc.so.6 /usr/lib/libc.so.6 /lib/libc.so.6; do
        if [ -f "$alt" ]; then
            log_info "Using alternative library: $alt"
            UPROBE_TARGET="$alt"
            return
        fi
    done

    # NixOS: find libc via ldd
    if command -v ldd &>/dev/null; then
        local nix_libc
        nix_libc=$(ldd /bin/sh 2>/dev/null | grep 'libc\.so' | awk '{print $3}' | head -1)
        if [ -n "$nix_libc" ] && [ -f "$nix_libc" ]; then
            log_info "Using NixOS library: $nix_libc"
            UPROBE_TARGET="$nix_libc"
            return
        fi
    fi

    log_error "Target library not found. Tried: $UPROBE_TARGET and alternatives"
    exit 1
}

# Check that function exists in the target
check_target_function() {
    log_info "Checking function $UPROBE_FN exists in $UPROBE_TARGET..."

    if ! command -v nm &>/dev/null; then
        log_fail "nm not available - cannot verify function exists"
        exit 1
    fi

    # Check dynamic symbols for the function (handles versioned symbols like malloc@@GLIBC_2.2.5)
    # Subshell disables pipefail to avoid SIGPIPE issues when grep exits early
    if (set +o pipefail; nm -D "$UPROBE_TARGET" 2>/dev/null | grep -qE "^[0-9a-f]+ [TW] ${UPROBE_FN}(@|\$)"); then
        log_info "Function $UPROBE_FN found in $UPROBE_TARGET"
    else
        log_fail "Function $UPROBE_FN not found in $UPROBE_TARGET"
        exit 1
    fi
}

# Ensure clean initial state
ensure_clean_state() {
    log_info "Ensuring clean initial state..."
    log_info "Using runtime directory: $RUNTIME_DIR"
    log_info "Using config: $CONFIG"
    log_info "Using target: $UPROBE_TARGET"
    log_info "Using function: $UPROBE_FN"

    # Clean up any previous run
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}

# Step 1: Load uprobe program from OCI image
load_program() {
    log_info "Step 1: Loading uprobe program from OCI image..."
    log_info "Image: $IMAGE"

    local output
    if ! output=$(bpfman program load image -o json --programs=uprobe:uprobe_counter --image-url="$IMAGE" 2>/dev/null); then
        log_fail "Failed to load program"
        # Re-run without redirecting stderr to show the error
        bpfman program load image -o json --programs=uprobe:uprobe_counter --image-url="$IMAGE" || true
        exit 1
    fi
    PROG_ID=$(echo "$output" | jq -r '.[0].record.program_id')

    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to parse program ID from output"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"

    # Verify program info
    local prog_type
    prog_type=$(echo "$output" | jq -r '.[0].record.load.program_type')
    assert_eq "uprobe" "$prog_type" "Managed program type should be uprobe"

    local kernel_type
    kernel_type=$(echo "$output" | jq -r '.[0].status.kernel.program_type')
    assert_eq "kprobe" "$kernel_type" "Kernel program type should be kprobe (uprobe is a variant)"

    local prog_name
    prog_name=$(echo "$output" | jq -r '.[0].status.kernel.name')
    assert_eq "uprobe_counter" "$prog_name" "Program name should be uprobe_counter"

    log_pass "Uprobe program loaded successfully"
}

# Step 2: Attach to user-space function
attach_uprobe() {
    log_info "Step 2: Attaching uprobe program to $UPROBE_FN in $UPROBE_TARGET..."

    local output
    if ! output=$(bpfman link attach uprobe --target "$UPROBE_TARGET" --fn-name "$UPROBE_FN" -o json "$PROG_ID" 2>/dev/null); then
        log_fail "Failed to attach uprobe"
        bpfman link attach uprobe --target "$UPROBE_TARGET" --fn-name "$UPROBE_FN" -o json "$PROG_ID" || true
        exit 1
    fi

    LINK_ID=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$LINK_ID" ]; then
        log_fail "Failed to parse link ID from output"
        echo "$output"
        exit 1
    fi
    log_info "Link ID: $LINK_ID"

    # Verify link details
    local link_type target fn_name
    link_type=$(echo "$output" | jq -r '.record.kind')
    target=$(echo "$output" | jq -r '.record.details.target')
    fn_name=$(echo "$output" | jq -r '.record.details.fn_name')

    assert_eq "uprobe" "$link_type" "Link type should be uprobe"
    assert_eq "$UPROBE_TARGET" "$target" "Target should be $UPROBE_TARGET"
    assert_eq "$UPROBE_FN" "$fn_name" "Function name should be $UPROBE_FN"

    log_pass "Uprobe program attached to $UPROBE_FN in $UPROBE_TARGET"
}

# Step 3: Verify link in list
verify_links() {
    log_info "Step 3: Verifying link via list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local uprobe_link_count
    uprobe_link_count=$(echo "$output" | jq '[.links[] | select(.kind == "uprobe")] | length')

    assert_eq "1" "$uprobe_link_count" "Should have 1 uprobe link"

    log_pass "Link verified"
}

# Step 4: Verify no dispatchers (uprobe is single-attach)
verify_no_dispatchers() {
    log_info "Step 4: Verifying no dispatchers (uprobe is single-attach)..."

    # Check database for dispatchers - should be none for uprobe
    local disp_count
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers;" 2>/dev/null || echo "0")

    assert_eq "0" "$disp_count" "Should have 0 dispatchers (uprobe is single-attach)"

    log_pass "No dispatchers verified (as expected for uprobe)"
}

# Step 5: Detach link
detach_link() {
    log_info "Step 5: Detaching uprobe link..."

    bpfman link detach "$LINK_ID" 2>&1
    local saved_link_id="$LINK_ID"
    LINK_ID=""  # Clear so cleanup doesn't try again

    # Verify link is gone
    local link_output uprobe_link_count
    link_output=$(bpfman link list -o json 2>&1)
    uprobe_link_count=$(echo "$link_output" | jq '[.links[] | select(.kind == "uprobe")] | length')
    assert_eq "0" "$uprobe_link_count" "Should have 0 uprobe links after detach"

    log_pass "Uprobe link detached"
}

# Step 6: Unload program
unload_program() {
    log_info "Step 6: Unloading uprobe program..."
    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""  # Clear so cleanup doesn't try again
    log_pass "Uprobe program unloaded"
}

# Step 7: Final verification
verify_final_state() {
    log_info "Step 7: Final verification..."

    # Check database - all zeros
    local prog_count uprobe_link_count
    prog_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM managed_programs;")
    uprobe_link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_uprobe_details;")

    assert_eq "0" "$prog_count" "Should have 0 programs"
    assert_eq "0" "$uprobe_link_count" "Should have 0 uprobe link details"

    log_pass "Final state verified - clean"
}

# Main
main() {
    echo "=========================================="
    echo "Uprobe Program Load/Attach Integration Test"
    echo "=========================================="
    echo ""

    check_target_library
    echo ""

    check_target_function
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_uprobe
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
    log_pass "All uprobe tests passed!"
    echo "=========================================="
}

main "$@"
