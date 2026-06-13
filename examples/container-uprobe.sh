#!/usr/bin/env bash
# examples/container-uprobe.sh
#
# Self-validating shell example for namespace-aware (container) uprobe
# attachment. It exercises the one-shot CLI boundary: the attach runs in
# one bpfman process, then fresh commands verify that the link was pinned
# and given a kernel bpf_link ID.
#
# Requires root and a built e2e.test binary (`make bin/e2e.test`).
# Run from the repo root with:
#   sudo ./examples/container-uprobe.sh

set -euo pipefail
shopt -s inherit_errexit

target_bin="$(pwd)/bin/e2e.test"
target_fn="e2e_uprobe_call_malloc"

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ -x "$target_bin" ]] || fail "missing $target_bin (run: make bin/e2e.test)"

unshare -m -- sleep 600 &
ns_pid=$!
cleanup() {
    [[ -n "${link:-}" ]]    && bpfman link detach "$link"      2>/dev/null || true
    [[ -n "${prog:-}" ]]    && bpfman program unload "$prog"    2>/dev/null || true
    [[ -n "${ns_pid:-}" ]]  && kill "$ns_pid"                   2>/dev/null || true
}
trap cleanup EXIT

self_ns=$(readlink "/proc/self/ns/mnt")
# unshare runs unshare(CLONE_NEWNS) a moment after the shell forks the
# child, so wait for the target's mount namespace to actually differ
# rather than racing the readlink against the switch.
targ_ns=$(readlink "/proc/$ns_pid/ns/mnt" 2>/dev/null || true)
for _ in $(seq 1 50); do
    [[ -n "$targ_ns" && "$targ_ns" != "$self_ns" ]] && break
    sleep 0.1
    targ_ns=$(readlink "/proc/$ns_pid/ns/mnt" 2>/dev/null || true)
done
[[ "$self_ns" != "$targ_ns" ]] || fail "target shares our mount namespace ($self_ns): not a real switch"
echo "target pid=$ns_pid in separate mount namespace ($targ_ns vs $self_ns)"

prog=$(bpfman program load file \
    e2e/testdata/bpf/uprobe_counter.bpf.o \
    --programs uprobe:uprobe_counter \
    -o jsonpath='{.programs[0].record.program_id}')
echo "loaded uprobe program $prog"

link=$(bpfman link attach uprobe \
    "$prog" \
    "$target_bin" \
    --fn-name "$target_fn" \
    --container-pid "$ns_pid" \
    -o jsonpath='{.record.id}')
echo "attached container uprobe link $link"

kernel_link_id=$(bpfman link get "$link" -o jsonpath='{.record.kernel_link_id}')
pin_path=$(bpfman link get "$link" -o jsonpath='{.record.pin_path}')

[[ "$kernel_link_id" != "" && "$kernel_link_id" != "null" ]] || fail "missing kernel link ID for link $link"
[[ "$pin_path" != "" && "$pin_path" != "null" ]] || fail "missing link pin path for link $link"
[[ -e "$pin_path" ]] || fail "link pin path does not exist: $pin_path"

bpftool link list | grep -q "^${kernel_link_id}:" || fail "kernel link $kernel_link_id not visible after attach command exited"

echo "kernel link id for link $link: $kernel_link_id"
echo "pin path for link $link: $pin_path"
bpfman link list

bpfman link detach "$link"
bpfman program unload "$prog"
link=""; prog=""
echo "cleaned up"
