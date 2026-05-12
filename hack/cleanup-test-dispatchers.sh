#!/usr/bin/env bash
#
# Drain dispatcher residue left on test interfaces by an interrupted
# e2e run. Targets only the harness's own namespace: test netns named
# `B<hex>N` and host-side veth interfaces matching the same pattern.
# For every interface inside that scope, removes any XDP program and
# any clsact qdisc unconditionally; whatever was there was either a
# bpfman dispatcher (the residue you want gone) or some other test
# attachment (which is equally fine to drop, since the harness owns
# the namespace).
#
# When and why to use:
#   - You ran the parallel-scripts harness and Ctrl-C'd it mid-flight.
#     The veths and netns are still there; some may still have a
#     dispatcher attached, which can keep its BPF program alive in the
#     kernel even after the harness DB has been wiped.
#   - You want the gentlest cleanup that still drains test residue:
#     this script touches nothing outside the B<hex>N namespace, so it
#     is safe to run on a host where a real bpfman daemon is managing
#     dispatchers on production interfaces.
#
# Pair with cleanup-test-veths.sh and cleanup-test-netns.sh. Run order
# is dispatchers first (this script), then veths, then netns; deleting
# the veth or netns cascades a final implicit drain, but doing this
# pass first leaves the kernel BPF tables clean even if the cascade
# misses a corner case.
#
# Use cleanup-all-dispatchers.sh instead when the residue is broader
# than the test namespace -- e.g. you killed the bpfman daemon itself
# and want every xdp_dispatcher / tc_dispatcher gone, regardless of
# which interface it lives on.
#
# Safe to run on a quiescent system. Idempotent: interfaces without
# XDP or without a clsact qdisc are skipped silently.
#
# Usage: sudo hack/cleanup-test-dispatchers.sh

set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "error: must be run as root (sudo $0)" >&2
    exit 1
fi

# drain_iface removes any XDP program and any clsact qdisc from the
# given interface, optionally inside a named netns. Both removals are
# best-effort; the calls fail silently when nothing is attached, which
# is the behaviour we want for an idempotent sweep.
drain_iface() {
    local ns="${1:-}"
    local iface="$2"
    local label="${ns:-host}"
    local ip_cmd=(ip)
    local tc_cmd=(tc)
    if [[ -n "$ns" ]]; then
        ip_cmd=(ip -n "$ns")
        tc_cmd=(ip netns exec "$ns" tc)
    fi
    if "${ip_cmd[@]}" link set dev "$iface" xdp off 2>/dev/null; then
        echo "drained xdp on $iface ($label)"
    fi
    if "${tc_cmd[@]}" qdisc del dev "$iface" clsact 2>/dev/null; then
        echo "drained clsact on $iface ($label)"
    fi
}

# ifaces_in lists every non-loopback interface in a netns. The netns
# argument is empty for the host; an explicit name routes the lookup
# through `ip -n`. Stripping the trailing peer suffix and the @ifindex
# suffix that `ip` appends gives us the bare interface name.
ifaces_in() {
    local ns="${1:-}"
    local ip_cmd=(ip)
    if [[ -n "$ns" ]]; then
        ip_cmd=(ip -n "$ns")
    fi
    "${ip_cmd[@]}" -o link show \
        | awk -F': ' '{print $2}' \
        | awk -F'@' '{print $1}' \
        | grep -vx 'lo' || true
}

# Test netns are the harness's auto-named `B<hex>N` slots; iterate via
# /run/netns rather than `ip netns list` so we always match the same
# filesystem-anchored set the other two cleanup scripts use.
shopt -s nullglob
for path in /run/netns/B*N; do
    ns=$(basename "$path")
    if [[ ! "$ns" =~ ^B[0-9a-f]+N$ ]]; then
        continue
    fi
    while read -r iface; do
        drain_iface "$ns" "$iface"
    done < <(ifaces_in "$ns")
done

# Host-side test veths share the same naming convention with a per-end
# suffix (`Babcdef123Na`, `Babcdef123Nb`). A dispatcher attached to
# either end before the netns or peer was cleaned up will sit here
# until something detaches it.
while read -r iface; do
    if [[ "$iface" =~ ^B[0-9a-f]+N[ab]?$ ]]; then
        drain_iface "" "$iface"
    fi
done < <(ifaces_in "")

echo "done"
