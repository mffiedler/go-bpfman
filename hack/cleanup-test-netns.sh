#!/usr/bin/env bash
#
# Remove stranded netns created by the e2e parallel-scripts harness.
# `net veth-pair` with auto-naming creates a netns whose name matches
# `B<hex>N`. If a script is interrupted before its
# `defer net release $pair` runs, the netns survives and blocks a
# later slot acquisition from re-creating the same name.
#
# Pair with cleanup-test-veths.sh; run veths first so the host-side
# delete reclaims the peer end before this script removes the netns
# the peer lived in.
#
# Safe to run on a quiescent system. Idempotent: missing netns are
# skipped silently.
#
# Usage: sudo hack/cleanup-test-netns.sh

set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "error: must be run as root (sudo $0)" >&2
    exit 1
fi

count=0
shopt -s nullglob
for path in /run/netns/B*N; do
    name=$(basename "$path")
    if [[ ${name} =~ ^B[0-9a-f]+N$ ]] && ip netns del "$name" 2>/dev/null; then
        echo "deleted netns $name"
        count=$((count + 1))
    fi
done

echo "removed ${count} stranded test netns"
