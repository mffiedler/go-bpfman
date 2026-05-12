#!/usr/bin/env bash
#
# Remove stranded host-side veth interfaces created by the e2e
# parallel-scripts harness. The harness uses `net veth-pair` with
# auto-naming; the generated link basenames have shape `B<hex>N`
# plus a per-end suffix (e.g. `Babcdef123Na`, `Babcdef123Nb`). When
# a script is interrupted before its `defer net release $pair` runs
# (Ctrl-C, kill, segfault), the veth survives and blocks a later
# slot acquisition from re-creating the same name.
#
# This script deletes every link whose ifname matches that pattern.
# Deleting the host end automatically reclaims the peer.
#
# Safe to run on a quiescent system. Idempotent: missing links are
# skipped silently.
#
# Usage: sudo hack/cleanup-test-veths.sh

set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "error: must be run as root (sudo $0)" >&2
    exit 1
fi

count=0
for iface in $(ip -o link show \
    | awk -F: '/^[0-9]+: B[0-9a-f]+N(:|a@|b@)/ {gsub(/[ a@b].*/, "", $2); print $2}' \
    | sort -u); do
    if ip link del "$iface" 2>/dev/null; then
        echo "deleted veth $iface"
        count=$((count + 1))
    fi
done

echo "removed ${count} stranded test veth interface(s)"
