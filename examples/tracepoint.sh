#!/usr/bin/env bash
# examples/tracepoint.sh
#
# Self-validating shell example for `bpfman program load` /
# `program get` / `program list` / `link attach` / `link get` /
# `link list`. Loads three tracepoint programs from one .bpf.o in
# a single load call, attaches each to a different syscall
# tracepoint, round-trips metadata and IDs through every CLI
# consumer, then detaches and unloads. Any inconsistency exits
# non-zero.
#
# Mirrors the `examples/tracepoint.bpfman` shell walkthrough but
# exercises the CLI from plain shell, validating that the wrapped
# `{"programs": [...]}` JSON shape and the load-order contract
# hold end-to-end for a non-shell-DSL consumer.
#
# This script intentionally relies only on `bpfman -o jsonpath=`
# for value extraction. No `jq`. The goal is to confirm that the
# CLI's structured-output surface is rich enough to drive shell
# automation by itself, and to flush out any gaps.
#
# Requires root. Run from the repo root with:
#   sudo ./examples/tracepoint.sh

set -euo pipefail
shopt -s inherit_errexit

run_label="tracepoint-shell-$$"

# Helpers ------------------------------------------------------------
fail() { echo "FAIL: $*" >&2; exit 1; }
assert_eq() {
    local got="$1" want="$2" desc="${3:-}"
    [[ "$got" == "$want" ]] || fail "${desc:-assert_eq}: got '$got', want '$want'"
}

# Load --------------------------------------------------------------
# Load three programs with shared metadata so a list filter can
# round-trip them as a set. Capture all three program IDs in one
# jsonpath sweep against the wrapped output.
ids=$(bpfman program load file \
    --path e2e/testdata/bpf/multi_prog_tracepoint_counter.bpf.o \
    --programs tracepoint:tp_a \
    --programs tracepoint:tp_b \
    --programs tracepoint:tp_c \
    -m "test=$run_label" \
    -m "kind=tracepoint" \
    -o jsonpath='{range .programs[*]}{.record.program_id} {end}')
read -r prog_a prog_b prog_c extra <<< "$ids"
[[ -z "${extra:-}" ]] || fail "load returned more IDs than requested: '$ids'"
echo "loaded: tp_a=$prog_a tp_b=$prog_b tp_c=$prog_c"

# program get round-trip --------------------------------------------
# For each program, fetch four fields in one jsonpath query and
# assert they match what we asked for. Uses a space-separated
# template that bash's `read` can split directly into vars.
expect_name=("tp_a" "tp_b" "tp_c")
prog_ids=("$prog_a" "$prog_b" "$prog_c")
for i in 0 1 2; do
    pid="${prog_ids[$i]}"
    name_want="${expect_name[$i]}"
    read -r got_name got_type got_test got_kind <<< "$(bpfman program get "$pid" \
        -o jsonpath='{.record.meta.name} {.record.load.program_type} {.record.meta.metadata.test} {.record.meta.metadata.kind}')"
    assert_eq "$got_name" "$name_want"     "program get $pid name"
    assert_eq "$got_type" "tracepoint"     "program get $pid type"
    assert_eq "$got_test" "$run_label"     "program get $pid metadata.test"
    assert_eq "$got_kind" "tracepoint"     "program get $pid metadata.kind"
done
echo "program get round-trip: ok"

# program list round-trip -------------------------------------------
# A label-selector list against our metadata must return exactly
# the three programs we just loaded, by ID, in any order.
list_ids=$(bpfman program list -l "test=$run_label" \
    -o jsonpath='{range .programs[*]}{.record.program_id}{"\n"}{end}' \
    | sort -n)
expect_ids=$(printf '%s\n' "$prog_a" "$prog_b" "$prog_c" | sort -n)
assert_eq "$list_ids" "$expect_ids" "program list filter by metadata"
echo "program list filter: ok"

# Attach ------------------------------------------------------------
link_a=$(bpfman link attach tracepoint "$prog_a" syscalls/sys_enter_kill   -o jsonpath='{.record.id}')
link_b=$(bpfman link attach tracepoint "$prog_b" syscalls/sys_enter_close  -o jsonpath='{.record.id}')
link_c=$(bpfman link attach tracepoint "$prog_c" syscalls/sys_enter_openat -o jsonpath='{.record.id}')
echo "attached: link_a=$link_a link_b=$link_b link_c=$link_c"

# link get round-trip -----------------------------------------------
# Each link's program_id, kind, and tracepoint coordinates must
# match what we attached.
expect_tp=("syscalls/sys_enter_kill" "syscalls/sys_enter_close" "syscalls/sys_enter_openat")
link_ids=("$link_a" "$link_b" "$link_c")
for i in 0 1 2; do
    lid="${link_ids[$i]}"
    read -r got_pid got_kind got_group got_name <<< "$(bpfman link get "$lid" \
        -o jsonpath='{.record.program_id} {.record.kind} {.record.details.group} {.record.details.name}')"
    assert_eq "$got_pid"  "${prog_ids[$i]}" "link get $lid program_id"
    assert_eq "$got_kind" "tracepoint"      "link get $lid kind"
    assert_eq "$got_group/$got_name" "${expect_tp[$i]}" "link get $lid tracepoint"
done
echo "link get round-trip: ok"

# link list filter --------------------------------------------------
# Filter by program_id should return exactly the link we attached
# to that program.
filtered=$(bpfman link list --program-id "$prog_a" \
    -o jsonpath='{range .links[*]}{.id}{"\n"}{end}')
assert_eq "$filtered" "$link_a" "link list --program-id $prog_a"
echo "link list filter: ok"

# Cleanup -----------------------------------------------------------
bpfman link detach "$link_a" "$link_b" "$link_c"
bpfman program unload "$prog_a" "$prog_b" "$prog_c"
echo "cleaned up"
