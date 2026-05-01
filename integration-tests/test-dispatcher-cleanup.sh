#!/bin/bash
# test-dispatcher-cleanup.sh - Test dispatcher lifecycle cleanup
#
# This test verifies that XDP dispatchers are automatically cleaned up
# when the last extension is detached.
#
# Test flow:
# 1. Load an XDP program
# 2. Attach repeatedly until max slots (10) are filled
# 3. Verify dispatcher state (interface, filesystem, database)
# 4. Detach all links, observing extension count decrement
# 5. Verify dispatcher is fully cleaned up after last detach
# 6. Unload program and verify final state
#
# Prerequisites:
# - bpfman binary built (bin/bpfman)
# - Root privileges (run with sudo)
# - SQLite3 installed
# - jq installed

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

# Configuration - can be overridden via environment
BPFMAN="${BPFMAN:-./bin/bpfman}"
CONFIG="${CONFIG:-./config/test.toml}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-integration-test-$$}"
IMAGE="${IMAGE:-quay.io/bpfman-bytecode/xdp_pass:latest}"
IFACE="bpfman-disp"

# Derived paths (matching RuntimeDirs structure)
DB_PATH="$RUNTIME_DIR/db/store.db"
BPFFS_ROOT="$RUNTIME_DIR/fs"
BPFFS_XDP="$BPFFS_ROOT/xdp"

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

    # Clean up any previous run
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true

    create_test_interface
}

# Step 1: Load XDP program
load_program() {
    log_info "Step 1: Loading XDP program..."
    log_info "Image: $IMAGE"
    PROG_ID=$(bpfman program load image -o 'jsonpath={.programs[0].record.program_id}' --programs=xdp:pass --image-url="$IMAGE" 2>&1)

    if [ -z "$PROG_ID" ] || [ "$PROG_ID" = "null" ]; then
        log_fail "Failed to load program"
        echo "$output"
        exit 1
    fi
    log_info "Loaded program ID: $PROG_ID"
}

# Step 2: Attach until max slots
attach_until_full() {
    log_info "Step 2: Attaching XDP until max slots..."
    LINK_IDS=()
    local max_slots=10

    for i in $(seq 1 $((max_slots + 2))); do
        local output
        local link_id
        link_id=$(bpfman link attach xdp --iface "$IFACE" -o 'jsonpath={.record.id}' "$PROG_ID" 2>/dev/null) || true

        if [ -z "$link_id" ]; then
            if [ "$i" -gt "$max_slots" ]; then
                log_info "Attach $i failed as expected (max slots reached)"
                break
            else
                log_fail "Attach $i failed unexpectedly"
                echo "$output"
                exit 1
            fi
        fi

        LINK_IDS+=("$link_id")
        log_info "Attach $i: kernel_link_id=$link_id"
    done

    assert_eq "$max_slots" "${#LINK_IDS[@]}" "Should have created exactly $max_slots links"
}

# Step 3: Verify dispatcher state
verify_dispatcher_state() {
    log_info "Step 3: Verifying dispatcher state..."

    # Check interface
    local xdp_info
    xdp_info=$(ip link show "$IFACE" | grep -o "prog/xdp id [0-9]*" || echo "none")
    if [ "$xdp_info" = "none" ]; then
        log_fail "No XDP program attached to $IFACE"
        exit 1
    fi
    log_info "Interface: $xdp_info"

    # Check filesystem
    if [ ! -d "$BPFFS_XDP" ]; then
        log_fail "XDP directory does not exist: $BPFFS_XDP"
        exit 1
    fi

    local link_pin="$BPFFS_XDP/dispatcher_"*"_link"
    if ! ls $link_pin >/dev/null 2>&1; then
        log_fail "Dispatcher link pin not found"
        exit 1
    fi
    log_info "Filesystem: dispatcher link and revision directory present"

    # Check database
    local link_count
    link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_xdp_details;")
    assert_eq "10" "$link_count" "Should have 10 XDP link details"

    local disp_count
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers WHERE type='xdp';")
    assert_eq "1" "$disp_count" "Should have 1 XDP dispatcher"

    log_info "Database: dispatcher count=$disp_count, links=$link_count"
}

# Step 4: Detach all links
detach_all_links() {
    log_info "Step 4: Detaching all links..."
    local expected_count=10

    for link_id in "${LINK_IDS[@]}"; do
        bpfman link detach "$link_id" 2>&1
        expected_count=$((expected_count - 1))

        local actual_count
        actual_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_xdp_details;" 2>/dev/null || echo "0")

        if [ "$expected_count" -eq 0 ]; then
            assert_eq "0" "$actual_count" "Should have 0 XDP link details after last detach"
            local disp_count
            disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers WHERE type='xdp';" 2>/dev/null || echo "0")
            assert_eq "0" "$disp_count" "Dispatcher should be deleted when all extensions removed"
            log_info "Detached link_id=$link_id -> Dispatcher DELETED"
        else
            assert_eq "$expected_count" "$actual_count" "Extension count mismatch after detach"
            log_info "Detached link_id=$link_id -> extensions: $actual_count"
        fi
    done
}

# Step 5: Unload program
unload_program() {
    log_info "Step 5: Unloading program..."
    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""  # Clear so cleanup doesn't try again
    log_info "Program unloaded"
}

# Step 6: Final verification
verify_final_state() {
    log_info "Step 6: Final verification..."

    # Check interface - no XDP
    local xdp_info
    xdp_info=$(ip link show "$IFACE" | grep -o "xdpgeneric" || echo "none")
    assert_eq "none" "$xdp_info" "Interface should have no XDP program"
    log_info "Interface: clean (no XDP)"

    # Check filesystem - empty
    local contents
    contents=$(ls "$BPFFS_XDP" 2>/dev/null | wc -l)
    assert_eq "0" "$contents" "XDP directory should be empty"
    log_info "Filesystem: clean (empty)"

    # Check database - all zeros
    local prog_count link_count disp_count xdp_count
    prog_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM managed_programs;")
    link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM links;")
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers;")
    xdp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_xdp_details;")

    assert_eq "0" "$prog_count" "Should have 0 programs"
    assert_eq "0" "$link_count" "Should have 0 links"
    assert_eq "0" "$disp_count" "Should have 0 dispatchers"
    assert_eq "0" "$xdp_count" "Should have 0 XDP details"

    log_info "Database: clean (all counts = 0)"
}

# Main
main() {
    echo "========================================"
    echo "Dispatcher Lifecycle Cleanup Test"
    echo "========================================"
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_until_full
    echo ""

    verify_dispatcher_state
    echo ""

    detach_all_links
    echo ""

    unload_program
    echo ""

    verify_final_state
    echo ""

    echo "========================================"
    log_pass "All tests passed!"
    echo "========================================"
}

main "$@"
