#!/bin/bash
# test-nsenter.sh - Functional test for container uprobe attachment
#
# This test verifies that we can attach a uprobe to a binary inside a
# container's mount namespace using the bpfman-ns helper.
#
# Prerequisites:
# - bpfman binary built with CGO (bin/bpfman)
# - Root privileges
# - A running container (docker or podman)

set -euo pipefail

BPFMAN="${BPFMAN:-./bin/bpfman}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
log_fail() { echo -e "${RED}[FAIL]${NC} $*"; }

cleanup() {
    if [[ -n "${CONTAINER_ID:-}" ]]; then
        log_info "Cleaning up container $CONTAINER_ID"
        docker rm -f "$CONTAINER_ID" &>/dev/null || true
    fi
}

trap cleanup EXIT

main() {
    echo "=========================================="
    echo "Container Uprobe Functional Test"
    echo "=========================================="
    echo ""

    cd "$PROJECT_DIR"

    # Check prerequisites
    if [[ ! -x "$BPFMAN" ]]; then
        log_error "bpfman binary not found at $BPFMAN"
        log_info "Run 'make bpfman-build' first"
        exit 1
    fi

    if ! command -v docker &>/dev/null; then
        log_error "docker not found"
        exit 1
    fi

    if [[ $EUID -ne 0 ]]; then
        log_error "This test requires root privileges"
        exit 1
    fi

    # Start a test container with a long-running process
    log_info "Starting test container..."
    CONTAINER_ID=$(docker run -d --rm alpine:latest sleep 3600)
    log_info "Container ID: $CONTAINER_ID"

    # Get container PID
    CONTAINER_PID=$(docker inspect -f '{{.State.Pid}}' "$CONTAINER_ID")
    log_info "Container PID: $CONTAINER_PID"

    # Verify namespace exists
    NS_PATH="/proc/$CONTAINER_PID/ns/mnt"
    if [[ ! -e "$NS_PATH" ]]; then
        log_fail "Namespace path does not exist: $NS_PATH"
        exit 1
    fi

    # Get namespace inodes for comparison
    HOST_NS_INODE=$(stat -c %i /proc/self/ns/mnt)
    CONTAINER_NS_INODE=$(stat -c %i "$NS_PATH")
    log_info "Host mount namespace inode: $HOST_NS_INODE"
    log_info "Container mount namespace inode: $CONTAINER_NS_INODE"

    if [[ "$HOST_NS_INODE" == "$CONTAINER_NS_INODE" ]]; then
        log_fail "Container is in the same mount namespace as host"
        exit 1
    fi
    log_pass "Container is in a different mount namespace"

    # Load a uprobe program
    UPROBE_OBJ="/home/aim/src/github.com/libbpf/libbpf-bootstrap/examples/c/uprobe.bpf.o"
    if [[ ! -f "$UPROBE_OBJ" ]]; then
        log_error "uprobe.bpf.o not found at $UPROBE_OBJ"
        exit 1
    fi

    log_info "Loading uprobe program from $UPROBE_OBJ..."
    LOAD_OUTPUT=$("$BPFMAN" program load file \
        --path "$UPROBE_OBJ" \
        --programs uprobe:uprobe \
        --metadata bpfman.io/test=nsenter 2>&1)

    PROGRAM_ID=$(echo "$LOAD_OUTPUT" | grep -oP 'Program ID:\s*\K[0-9]+' | head -1)
    if [[ -z "$PROGRAM_ID" ]]; then
        log_fail "Failed to load program or parse ID"
        echo "$LOAD_OUTPUT"
        exit 1
    fi
    log_pass "Loaded program ID: $PROGRAM_ID"

    # Attach uprobe to libc in container namespace
    # Alpine uses musl libc at /lib/ld-musl-x86_64.so.1
    log_info "Attaching uprobe to container's libc..."
    ATTACH_OUTPUT=$("$BPFMAN" link attach uprobe \
        --target /lib/ld-musl-x86_64.so.1 \
        --fn-name malloc \
        --container-pid "$CONTAINER_PID" \
        "$PROGRAM_ID" 2>&1) || {
        log_fail "Failed to attach uprobe"
        echo "$ATTACH_OUTPUT"
        "$BPFMAN" program unload "$PROGRAM_ID" || true
        exit 1
    }

    LINK_ID=$(echo "$ATTACH_OUTPUT" | grep -oP 'Link ID:\s*\K[0-9]+' | head -1)
    if [[ -z "$LINK_ID" ]]; then
        log_warn "Could not parse link ID from output (may be using fd-based link)"
        echo "$ATTACH_OUTPUT"
    else
        log_pass "Attached link ID: $LINK_ID"
    fi

    # Verify attachment by listing links
    log_info "Verifying attachment..."
    LIST_OUTPUT=$("$BPFMAN" link list 2>&1)
    echo "$LIST_OUTPUT"

    # Clean up
    log_info "Cleaning up..."
    if [[ -n "${LINK_ID:-}" ]]; then
        "$BPFMAN" link detach "$LINK_ID" || true
    fi
    "$BPFMAN" program unload "$PROGRAM_ID" || true

    echo ""
    echo "=========================================="
    log_pass "Container uprobe test completed!"
    echo "=========================================="
}

main "$@"
