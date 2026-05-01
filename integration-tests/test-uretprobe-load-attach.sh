#!/bin/bash
# test-uretprobe-load-attach.sh - Test uretprobe program loading and attachment
#
# This test verifies that uretprobe programs can be loaded and attached.
# Uretprobe shares the uprobe implementation but attaches to function return.
#
# The key difference from uprobe:
# - Load with --programs uretprobe:func_name
# - The link type should show as "uretprobe" (not "uprobe")
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
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-uretprobe-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/go-uprobe-counter:latest}"
UPROBE_TARGET="${UPROBE_TARGET:-/lib64/libc.so.6}"
UPROBE_FN="${UPROBE_FN:-malloc}"

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

    log_fail "Target library not found. Tried: $UPROBE_TARGET and alternatives"
    exit 1
}

ensure_clean_state() {
    log_info "Ensuring clean initial state..."
    log_info "Using runtime directory: $RUNTIME_DIR"
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}

# Step 1: Load as URETPROBE (not uprobe)
load_program() {
    log_info "Step 1: Loading program as URETPROBE type..."
    log_info "Image: $IMAGE"
    log_info "Using: --programs uretprobe:uprobe_counter"

    local output
    if ! output=$(bpfman program load image -o json --programs=uretprobe:uprobe_counter --image-url="$IMAGE" 2>/dev/null); then
        log_fail "Failed to load program"
        bpfman program load image -o json --programs=uretprobe:uprobe_counter --image-url="$IMAGE" || true
        exit 1
    fi
    PROG_ID=$(echo "$output" | jq -r '.programs[0].record.program_id')

    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to parse program ID from output"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"

    # Verify program type is uretprobe (this is the key check!)
    local stored_type
    stored_type=$(echo "$output" | jq -r '.programs[0].record.load.program_type')
    log_info "Program type from managed metadata: $stored_type"
    assert_eq "uretprobe" "$stored_type" "Managed program type should be uretprobe"

    log_pass "Uretprobe program loaded successfully"
}

# Step 2: Attach (using uprobe command - retprobe derived from program type)
attach_uretprobe() {
    log_info "Step 2: Attaching uretprobe program to $UPROBE_FN in $UPROBE_TARGET..."
    log_info "Note: Using 'attach <id> uprobe' - retprobe flag derived from program type"

    local output
    if ! output=$(bpfman link attach uprobe --target "$UPROBE_TARGET" --fn-name "$UPROBE_FN" -o json "$PROG_ID" 2>/dev/null); then
        log_fail "Failed to attach uretprobe"
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

    # THE KEY CHECK: Link type should be "uretprobe" not "uprobe"
    local link_type
    link_type=$(echo "$output" | jq -r '.record.kind')
    log_info "Link type: $link_type"
    assert_eq "uretprobe" "$link_type" "Link type should be uretprobe (not uprobe)"

    # Verify details
    local target fn_name retprobe_flag
    target=$(echo "$output" | jq -r '.record.details.target')
    fn_name=$(echo "$output" | jq -r '.record.details.fn_name')
    retprobe_flag=$(echo "$output" | jq -r '.record.details.retprobe')

    assert_eq "$UPROBE_TARGET" "$target" "Target should match"
    assert_eq "$UPROBE_FN" "$fn_name" "Function name should match"
    assert_eq "true" "$retprobe_flag" "Retprobe flag should be true"

    log_pass "Uretprobe attached successfully with correct link type"
}

# Step 3: Verify link in list shows as uretprobe
verify_links() {
    log_info "Step 3: Verifying link type in list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local uretprobe_count
    uretprobe_count=$(echo "$output" | jq '[.links[] | select(.kind == "uretprobe")] | length')
    assert_eq "1" "$uretprobe_count" "Should have 1 uretprobe link"

    # Also verify no uprobe links (to ensure it's not misclassified)
    local uprobe_count
    uprobe_count=$(echo "$output" | jq '[.links[] | select(.kind == "uprobe")] | length')
    assert_eq "0" "$uprobe_count" "Should have 0 uprobe links (it's a uretprobe)"

    log_pass "Link correctly shows as uretprobe type"
}

# Step 4: Check database
verify_database() {
    log_info "Step 4: Verifying database entries..."

    # Check link_registry has correct type
    local link_type
    link_type=$(sqlite3 "$DB_PATH" "SELECT kind FROM links WHERE link_id = $LINK_ID;")
    assert_eq "uretprobe" "$link_type" "Database link_type should be uretprobe"

    # Check uprobe_link_details has retprobe=1
    local retprobe_val
    retprobe_val=$(sqlite3 "$DB_PATH" "SELECT retprobe FROM link_uprobe_details WHERE link_id = $LINK_ID;")
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
    echo "============================================="
    echo "Uretprobe Program Load/Attach Integration Test"
    echo "============================================="
    echo ""

    check_target_library
    echo ""

    check_target_function
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_uretprobe
    echo ""

    verify_links
    echo ""

    verify_database
    echo ""

    cleanup_test
    echo ""

    echo "============================================="
    log_pass "All uretprobe tests passed!"
    echo "============================================="
}

main "$@"
