#!/bin/bash
#
# Integration test for bpfman gRPC API using grpcurl.
# Requires: docker, quay.io/bpfman/bpfman:latest image available locally
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROTO_DIR="$(dirname "$SCRIPT_DIR")/proto"
SOCKET_DIR="/run/bpfman-sock"
SOCKET_PATH="$SOCKET_DIR/bpfman.sock"
CONTAINER_NAME="bpfman-grpc-test"
GRPCURL_IMAGE="fullstorydev/grpcurl"
BPFMAN_IMG="${BPFMAN_IMG:-quay.io/bpfman/bpfman:latest}"

cleanup() {
    echo "Cleaning up..."
    docker stop "$CONTAINER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

grpcurl_cmd() {
    docker run --rm --user root \
        -v "$SOCKET_DIR:$SOCKET_DIR" \
        -v "$PROTO_DIR:/proto:ro" \
        "$GRPCURL_IMAGE" -plaintext -import-path /proto -proto bpfman.proto \
        "unix:$SOCKET_PATH" "$@"
}

# Ensure bpffs is mounted on the host
if ! mount | grep -q "^bpf on /sys/fs/bpf"; then
    echo "Mounting bpffs on host..."
    sudo mount -t bpf bpf /sys/fs/bpf || {
        echo "ERROR: Failed to mount bpffs. Run: sudo mount -t bpf bpf /sys/fs/bpf"
        exit 1
    }
fi

# Get the bpf group ID from the bpffs mount
BPF_GID=$(stat -c '%g' /sys/fs/bpf)

echo "=== Starting bpfman server ==="
docker run -d --name "$CONTAINER_NAME" --rm --privileged \
    --group-add "$BPF_GID" \
    -v /sys/fs/bpf:/sys/fs/bpf:rshared \
    -v "$SOCKET_DIR:$SOCKET_DIR" \
    "$BPFMAN_IMG" serve

echo "Waiting for server to start..."
sleep 2

echo ""
echo "=== Test: List services ==="
grpcurl_cmd list
echo "OK"

echo ""
echo "=== Test: List methods ==="
grpcurl_cmd list bpfman.v1.Bpfman
echo "OK"

echo ""
echo "=== Test: Load program ==="
LOAD_REQUEST='{"bytecode": {"file": "/opt/bpf/stats.o"}, "info": [{"name": "count_context_switches", "program_type": "TRACEPOINT"}]}'
LOAD_RESPONSE=$(docker run --rm --user root \
    -v "$SOCKET_DIR:$SOCKET_DIR" \
    -v "$PROTO_DIR:/proto:ro" \
    "$GRPCURL_IMAGE" -plaintext -import-path /proto -proto bpfman.proto \
    -d "$LOAD_REQUEST" \
    "unix:$SOCKET_PATH" bpfman.v1.Bpfman/Load)
echo "$LOAD_RESPONSE"

PROGRAM_ID=$(echo "$LOAD_RESPONSE" | jq -r '.programs[0].kernelInfo.id')
if [[ -z "$PROGRAM_ID" || "$PROGRAM_ID" == "null" ]]; then
    echo "FAILED: Could not extract program ID from Load response"
    exit 1
fi
echo "Loaded program ID: $PROGRAM_ID"
echo "OK"

echo ""
echo "=== Test: List programs ==="
LIST_RESPONSE=$(grpcurl_cmd bpfman.v1.Bpfman/List)
echo "$LIST_RESPONSE"

LISTED_ID=$(echo "$LIST_RESPONSE" | jq -r '.results[0].kernelInfo.id')
if [[ "$LISTED_ID" != "$PROGRAM_ID" ]]; then
    echo "FAILED: Listed program ID ($LISTED_ID) doesn't match loaded ID ($PROGRAM_ID)"
    exit 1
fi
echo "OK"

echo ""
echo "=== Test: Get program ==="
GET_RESPONSE=$(docker run --rm --user root \
    -v "$SOCKET_DIR:$SOCKET_DIR" \
    -v "$PROTO_DIR:/proto:ro" \
    "$GRPCURL_IMAGE" -plaintext -import-path /proto -proto bpfman.proto \
    -d "{\"id\": $PROGRAM_ID}" \
    "unix:$SOCKET_PATH" bpfman.v1.Bpfman/Get)
echo "$GET_RESPONSE"

GOT_ID=$(echo "$GET_RESPONSE" | jq -r '.kernelInfo.id')
if [[ "$GOT_ID" != "$PROGRAM_ID" ]]; then
    echo "FAILED: Got program ID ($GOT_ID) doesn't match loaded ID ($PROGRAM_ID)"
    exit 1
fi
echo "OK"

echo ""
echo "=== Test: Unload program ==="
docker run --rm --user root \
    -v "$SOCKET_DIR:$SOCKET_DIR" \
    -v "$PROTO_DIR:/proto:ro" \
    "$GRPCURL_IMAGE" -plaintext -import-path /proto -proto bpfman.proto \
    -d "{\"id\": $PROGRAM_ID}" \
    "unix:$SOCKET_PATH" bpfman.v1.Bpfman/Unload
echo "OK"

echo ""
echo "=== Test: Verify unload (List should be empty) ==="
LIST_AFTER=$(grpcurl_cmd bpfman.v1.Bpfman/List)
echo "$LIST_AFTER"

if [[ $(echo "$LIST_AFTER" | jq -r '.results // [] | length') != "0" ]]; then
    echo "FAILED: Program list should be empty after unload"
    exit 1
fi
echo "OK"

echo ""
echo "=== All tests passed ==="
