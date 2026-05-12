#!/usr/bin/env bash
#
# Drain every bpfman dispatcher attached anywhere on the host. Walks
# the root netns plus every netns under /run/netns, looks up each
# interface's XDP and TC attachments via `bpftool net show`, and for
# every attachment whose backing program is `xdp_dispatcher` or
# `tc_dispatcher` detaches it (XDP via `ip link set ... xdp off`, TC
# via `tc qdisc del ... clsact`). The kernel then garbage-collects
# the program once its last reference drops, unless something else
# (a pinned file, an open fd) keeps it alive.
#
# When and why to use:
#   - You killed the bpfman daemon or wiped its DB, and the kernel
#     still carries dispatcher programs whose owning links you no
#     longer have any handle on. `bpfman dispatcher list` shows
#     nothing (the DB is empty) but `bpftool prog show | grep
#     dispatcher` still shows them. This script is the blunt
#     instrument that drains them.
#   - You want a known-clean kernel state before re-running a test
#     suite, even if some residue is unrelated to the parallel-scripts
#     harness (e.g. left over from a manual `bpfman link attach`).
#
# Why this and not cleanup-test-dispatchers.sh:
#   - cleanup-test-dispatchers.sh only touches interfaces inside the
#     harness's `B<hex>N` namespace and removes XDP / clsact
#     unconditionally, accepting that anything in that scope is the
#     harness's. It is the right tool when you only want to clear
#     test residue and might have a real bpfman daemon running on
#     production interfaces you must not touch.
#   - This script is name-filtered (only programs literally named
#     xdp_dispatcher / tc_dispatcher are drained) so it can safely
#     run on a host where other XDP / TC programs are also present.
#     The cost is that it depends on bpftool and on those exact
#     program names; a renamed dispatcher would be missed.
#
# Safe to run on a quiescent system. Idempotent: a second invocation
# with nothing to drain prints "done" with no output above it.
#
# Usage: sudo hack/cleanup-all-dispatchers.sh

set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "error: must be run as root (sudo $0)" >&2
    exit 1
fi

for tool in bpftool jq ip tc; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "error: required tool '$tool' not found in PATH" >&2
        exit 1
    fi
done

# prog_name_is returns 0 when the program at the given id has the
# expected name, 1 otherwise. Used to distinguish a real dispatcher
# attachment from any other XDP / TC program that happens to be
# attached to the same interface.
prog_name_is() {
    local id="$1"
    local want="$2"
    local got
    got=$(bpftool prog show id "$id" -j 2>/dev/null | jq -r '.name // empty')
    [[ "$got" == "$want" ]]
}

# drain_netns walks every interface in one netns and detaches any
# attached bpfman dispatcher. The netns argument is empty for the
# root netns; an explicit name routes bpftool / ip / tc through
# `ip netns exec`.
drain_netns() {
    local ns="${1:-}"
    local label="${ns:-host}"
    local nsenter=()
    local ip_cmd=(ip)
    local tc_cmd=(tc)
    if [[ -n "$ns" ]]; then
        nsenter=(ip netns exec "$ns")
        ip_cmd=(ip -n "$ns")
        tc_cmd=("${nsenter[@]}" tc)
    fi

    local net_json
    net_json=$("${nsenter[@]}" bpftool net show -j 2>/dev/null || echo '[]')
    if [[ -z "$net_json" || "$net_json" == "[]" ]]; then
        return
    fi

    # XDP entries carry the prog id but not the name; look the name
    # up per id so we only drain interfaces whose XDP is actually a
    # bpfman dispatcher.
    while read -r ifname id; do
        [[ -z "$ifname" || -z "$id" ]] && continue
        if prog_name_is "$id" "xdp_dispatcher"; then
            if "${ip_cmd[@]}" link set dev "$ifname" xdp off 2>/dev/null; then
                echo "drained xdp_dispatcher on $ifname ($label)"
            fi
        fi
    done < <(echo "$net_json" | jq -r '.[0].xdp[]? | "\(.devname) \(.id)"')

    # TC entries already include the program name, so the name check
    # is local. A single clsact qdisc carries both ingress and egress
    # filters; removing it cascades both, so we deduplicate by ifname
    # to avoid a redundant second `tc qdisc del`.
    local seen=()
    while read -r ifname name; do
        [[ -z "$ifname" || -z "$name" ]] && continue
        if [[ "$name" != "tc_dispatcher" ]]; then
            continue
        fi
        local already=0
        for s in "${seen[@]:-}"; do
            if [[ "$s" == "$ifname" ]]; then
                already=1
                break
            fi
        done
        if [[ "$already" -eq 1 ]]; then
            continue
        fi
        seen+=("$ifname")
        if "${tc_cmd[@]}" qdisc del dev "$ifname" clsact 2>/dev/null; then
            echo "drained tc_dispatcher on $ifname ($label)"
        fi
    done < <(echo "$net_json" | jq -r '.[0].tc[]? | "\(.devname) \(.name)"')
}

drain_netns ""

shopt -s nullglob
for path in /run/netns/*; do
    drain_netns "$(basename "$path")"
done

echo "done"
