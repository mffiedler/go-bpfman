#!/bin/bash
# test-fentry-load-attach.sh - Test fentry program loading and attachment
#
# This test verifies that fentry programs can be:
# 1. Loaded from an OCI image (with attach function specified at load time)
# 2. Attached to a kernel function
# 3. Detached cleanly
# 4. Unloaded cleanly
#
# Fentry is a single-attach program type that attaches to kernel function entry.
# Unlike kprobe, the attach function is specified at load time, not attach time.
#
# Prerequisites:
# - bpfman binary built (bin/bpfman)
# - Root privileges (run with sudo)
# - SQLite3 installed
# - jq installed
# - config/test.toml present (with signature verification disabled)
# - Kernel with BTF support

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "This test must be run as root (sudo $0)" >&2
    exit 1
fi

# Configuration - can be overridden via environment
BPFMAN="${BPFMAN:-./bin/bpfman}"
CONFIG="${CONFIG:-./config/test.toml}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/bpfman-fentry-test-$$}"
# Script directory for finding bytecode
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Bytecode file containing fentry program
BYTECODE="${BYTECODE:-$SCRIPT_DIR/bytecode/fentry.bpf.o}"
# Kernel function to attach to - do_unlinkat is commonly used for fentry tests
FENTRY_FN="${FENTRY_FN:-do_unlinkat}"
# BPF function name in the bytecode
BPF_FUNC="${BPF_FUNC:-test_fentry}"

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

# Check that kernel function exists and BTF is available
check_prerequisites() {
    log_info "Checking prerequisites..."

    # Check BTF support
    if [ ! -f /sys/kernel/btf/vmlinux ]; then
        log_error "Kernel BTF not available at /sys/kernel/btf/vmlinux"
        log_error "Fentry requires a kernel with BTF support"
        exit 1
    fi
    log_info "Kernel BTF available"

    # Check kernel function exists
    if [ -f /proc/kallsyms ]; then
        if grep -q " ${FENTRY_FN}$" /proc/kallsyms 2>/dev/null || \
           grep -q " ${FENTRY_FN}\." /proc/kallsyms 2>/dev/null; then
            log_info "Kernel function $FENTRY_FN found"
        else
            log_warn "Kernel function $FENTRY_FN not found in /proc/kallsyms"
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
    log_info "Using bytecode: $BYTECODE"
    log_info "Using kernel function: $FENTRY_FN"
    log_info "Using BPF function: $BPF_FUNC"

    # Clean up any previous run
    if mountpoint -q "$BPFFS_ROOT" 2>/dev/null; then
        umount "$BPFFS_ROOT" 2>/dev/null || true
    fi
    rm -rf "$RUNTIME_DIR" "${RUNTIME_DIR}-sock" 2>/dev/null || true
}

# Step 1: Load fentry program from bytecode file
# Note: fentry programs specify the attach function at load time
load_program() {
    log_info "Step 1: Loading fentry program from bytecode file..."
    log_info "Bytecode: $BYTECODE"
    log_info "Program spec: fentry:${BPF_FUNC}:${FENTRY_FN}"

    # Check bytecode file exists
    if [ ! -f "$BYTECODE" ]; then
        log_fail "Bytecode file not found: $BYTECODE"
        log_info "You may need to compile it with: clang -O2 -g -target bpf -c fentry.bpf.c -o fentry.bpf.o"
        exit 1
    fi

    local output
    # For fentry, the format is: fentry:<bpf_func_name>:<kernel_attach_func>
    output=$(bpfman program load file -o json --programs="fentry:${BPF_FUNC}:${FENTRY_FN}" --path="$BYTECODE" 2>&1)
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
    assert_eq "fentry" "$prog_type" "Managed program type should be fentry"

    local kernel_type
    kernel_type=$(echo "$output" | jq -r '.[0].status.kernel.program_type')
    assert_eq "tracing" "$kernel_type" "Kernel program type should be tracing"

    local prog_name
    prog_name=$(echo "$output" | jq -r '.[0].status.kernel.name')
    assert_eq "$BPF_FUNC" "$prog_name" "Program name should be $BPF_FUNC"

    log_pass "Fentry program loaded successfully"
}

# Step 2: Attach to kernel function
# Note: No --fn-name needed - it was specified at load time
attach_fentry() {
    log_info "Step 2: Attaching fentry program..."
    log_info "Attach function was specified at load time: $FENTRY_FN"

    local output
    # For fentry, we don't pass --fn-name - it's stored from load time
    output=$(bpfman link attach fentry -o json "$PROG_ID" 2>&1)

    LINK_ID=$(echo "$output" | jq -r '.record.id // empty' 2>/dev/null) || true

    if [ -z "$LINK_ID" ]; then
        log_fail "Failed to attach fentry"
        echo "$output"
        exit 1
    fi
    log_info "Link ID: $LINK_ID"

    # Verify link details
    local link_type fn_name
    link_type=$(echo "$output" | jq -r '.record.kind')
    fn_name=$(echo "$output" | jq -r '.record.details.fn_name')

    assert_eq "fentry" "$link_type" "Link type should be fentry"
    assert_eq "$FENTRY_FN" "$fn_name" "Function name should be $FENTRY_FN"

    log_pass "Fentry program attached to $FENTRY_FN"
}

# Step 3: Verify link in list
verify_links() {
    log_info "Step 3: Verifying link via list..."

    local output
    output=$(bpfman link list -o json 2>&1)

    local fentry_link_count
    fentry_link_count=$(echo "$output" | jq '[.links[] | select(.kind == "fentry")] | length')

    assert_eq "1" "$fentry_link_count" "Should have 1 fentry link"

    log_pass "Link verified"
}

# Step 4: Verify no dispatchers (fentry is single-attach)
verify_no_dispatchers() {
    log_info "Step 4: Verifying no dispatchers (fentry is single-attach)..."

    # Check database for dispatchers - should be none for fentry
    local disp_count
    disp_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM dispatchers;" 2>/dev/null || echo "0")

    assert_eq "0" "$disp_count" "Should have 0 dispatchers (fentry is single-attach)"

    log_pass "No dispatchers verified (as expected for fentry)"
}

# Step 5: Detach link
detach_link() {
    log_info "Step 5: Detaching fentry link..."

    bpfman link detach "$LINK_ID" 2>&1
    local saved_link_id="$LINK_ID"
    LINK_ID=""  # Clear so cleanup doesn't try again

    # Verify link is gone
    local link_output fentry_link_count
    link_output=$(bpfman link list -o json 2>&1)
    fentry_link_count=$(echo "$link_output" | jq '[.links[] | select(.kind == "fentry")] | length')
    assert_eq "0" "$fentry_link_count" "Should have 0 fentry links after detach"

    log_pass "Fentry link detached"
}

# Step 6: Unload program
unload_program() {
    log_info "Step 6: Unloading fentry program..."
    bpfman program unload "$PROG_ID" 2>&1
    PROG_ID=""  # Clear so cleanup doesn't try again
    log_pass "Fentry program unloaded"
}

# Step 7: Final verification
verify_final_state() {
    log_info "Step 7: Final verification..."

    # Check database - all zeros
    local prog_count fentry_link_count
    prog_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM managed_programs;")
    fentry_link_count=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM link_fentry_details;")

    assert_eq "0" "$prog_count" "Should have 0 programs"
    assert_eq "0" "$fentry_link_count" "Should have 0 fentry link details"

    log_pass "Final state verified - clean"
}

# Main
main() {
    echo "=========================================="
    echo "Fentry Program Load/Attach Integration Test"
    echo "=========================================="
    echo ""

    check_prerequisites
    echo ""

    ensure_clean_state
    echo ""

    load_program
    echo ""

    attach_fentry
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
    log_pass "All fentry tests passed!"
    echo "=========================================="
}

main "$@"
