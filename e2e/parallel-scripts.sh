#!/usr/bin/env bash
#
# parallel-scripts.sh -- run the e2e/scripts/ .bpfman corpus in
# parallel against the built bpfman-shell binary.
#
# Every script in e2e/scripts/ uses the address-pool-backed
# `net veth-pair` builtin (see cmd/bpfman-shell/shell/netpool.go),
# so parallel runs are safe by construction.
#
# Each script runs from e2e/ so testdata paths match the Go e2e
# tests. This harness is the right place to exercise the pool's
# cross-process exclusion under realistic concurrent load.
#
# Style follows ~/nix-config/.../bin/dop: GNU parallel with --bar
# --eta --joblog, per-job log files, and a failure summary parsed
# out of the joblog at the end.
#
# Invocation: this harness must be run under sudo because every
# script touches kernel namespaces and BPF state. The wrapper
# does not embed sudo itself -- the caller is responsible for
# elevation so the build artefacts and direnv environment stay
# attributable to the invoking user:
#
#     direnv exec . sudo -E e2e/parallel-scripts.sh [-j N] [-f FILTER]
#
# (-E preserves the flake-provided PATH so `parallel` resolves.)
#
# Usage: parallel-scripts.sh [-j NUM] [-f FILTER] [-r N] [-F] [-- [parallel-args...]]
#
#   -j NUM     Number of parallel jobs (default: nproc).
#   -f FILTER  Substring match against the script basename; only
#              matching scripts are dispatched.
#   -r N       Run each script N times (default: 1). Useful for
#              stress-testing the address pool's cross-process
#              exclusion under sustained concurrent load -- e.g.
#              -r 100 turns 20 scripts into 2000 jobs.
#   -F, --fail-fast
#              Stop on the first failing job. Translates to GNU
#              parallel's `--halt now,fail=1` -- running jobs are
#              killed, no further jobs are scheduled, and the
#              harness exits non-zero. Default is to run every
#              job and produce a summary, which is what you want
#              for triage; -F is what you want when you're
#              already triaging one bug and do not need a second.
#   --         Everything after -- is passed through to GNU parallel.
#   -h         Show help and exit.
#
# Environment:
#   BIN_DIR    Directory containing the built bpfman-shell binary
#              (default: bin/, relative to the repo root).
#
# Exit codes:
#   0  every script passed
#   1  usage error or harness failure
#   2  one or more scripts failed; see the printed summary

set -eu
set -o pipefail

show_help() {
    sed -n '3,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

jobs=
filter=
repeats=1
fail_fast=0
parallel_passthrough=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)
            show_help
            exit 0
            ;;
        -j|--jobs)
            jobs="$2"
            shift 2
            ;;
        -f|--filter)
            filter="$2"
            shift 2
            ;;
        -r|--repeats)
            repeats="$2"
            shift 2
            ;;
        -F|--fail-fast)
            fail_fast=1
            shift
            ;;
        --)
            shift
            parallel_passthrough=("$@")
            break
            ;;
        *)
            echo "unknown option: $1" >&2
            show_help >&2
            exit 1
            ;;
    esac
done

if [[ -z "$jobs" ]]; then
    jobs="$(nproc)"
fi

# Cap at the veth address pool size. The pool carves
# 198.51.100.0/24 into 64 /30 subnets (PoolSize in
# cmd/bpfman-shell/shell/netpool.go), so any -j above that runs
# out of subnets and fails the late jobs with "more than 64
# concurrent pairs in flight" -- not a real test failure, just a
# resource limit. Clamping here so a 192-core arm node does not
# trip on this unexpectedly.
pool_cap=64
if (( jobs > pool_cap )); then
    echo "info: -j$jobs exceeds veth pool size ($pool_cap); clamping to -j$pool_cap" >&2
    jobs=$pool_cap
fi

if ! [[ "$repeats" =~ ^[1-9][0-9]*$ ]]; then
    echo "error: -r/--repeats must be a positive integer (got: $repeats)" >&2
    exit 1
fi

# Resolve paths. The script lives in e2e/; the repo root is the
# parent. The harness cd's into e2e/ before dispatching scripts
# so the .bpfman paths can be passed unmodified.
e2e_dir=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$e2e_dir/.." && pwd)
bin_dir=${BIN_DIR:-$repo_root/bin}
shell_bin="$bin_dir/bpfman-shell"

if [[ ! -x "$shell_bin" ]]; then
    echo "error: $shell_bin is not executable; run 'make' first" >&2
    exit 1
fi

if ! command -v parallel >/dev/null 2>&1; then
    echo "error: GNU parallel is not on PATH" >&2
    exit 1
fi

if [[ $EUID -ne 0 ]]; then
    echo "error: this harness must run as root (every script needs CAP_NET_ADMIN, CAP_SYS_ADMIN, etc.)" >&2
    echo "       try: direnv exec . sudo -E $0 $*" >&2
    exit 1
fi

# Per-run log directory; deleted and recreated so a previous run's
# logs do not bleed into the failure summary. Mirrors dop's
# layout: one log file per job, indexed by the joblog Seq column.
logdir=$(mktemp -d -t bpfman-parallel-scripts.XXXXXX)
joblog="$logdir/joblog.txt"
trap 'echo "logs: $logdir"' EXIT

# Build the script list. Each entry is a relative path under
# e2e/ (e.g. "scripts/TestMultiProgTC_AllProceed_CustomProceedOn.bpfman").
# Filtering is a substring match on the basename so callers can
# narrow with the same convention test-e2e-scripts.sh uses.
mapfile -t scripts < <(
    cd "$e2e_dir" && \
    for f in scripts/*.bpfman; do
        [[ -e "$f" ]] || continue
        if [[ -n "$filter" ]] && ! echo "$(basename "$f")" | grep -q -- "$filter"; then
            continue
        fi
        echo "$f"
    done
)

if [[ ${#scripts[@]} -eq 0 ]]; then
    echo "no .bpfman scripts matched filter=${filter:-<none>}" >&2
    exit 1
fi

# Expand scripts[] by the repeat factor. The expanded array is
# what we feed to parallel and what the failure summary indexes
# into by Seq. Layout is one full pass through scripts[] per
# repeat, so consecutive jobs run different scripts before any
# script is repeated -- this gives the address pool maximum
# diversity of names per wave, which is the load shape we want
# to stress.
expanded=()
for ((r = 0; r < repeats; r++)); do
    expanded+=("${scripts[@]}")
done

if [[ "$repeats" -gt 1 ]]; then
    echo "dispatching ${#scripts[@]} scripts x $repeats repeats = ${#expanded[@]} jobs with -j$jobs; logs under $logdir"
else
    echo "dispatching ${#scripts[@]} scripts with -j$jobs; logs under $logdir"
fi

# Choose the progress UI based on whether we have a usable
# terminal. --bar / --eta render correctly through /dev/tty when
# the harness runs interactively; without a TTY they fall back to
# escape-code spam in captured logs. The polite non-interactive
# default is to run silently and let the summary do the talking.
progress_args=()
if [[ -t 1 ]] && [[ -r /dev/tty ]] && [[ -w /dev/tty ]]; then
    progress_args+=(--bar --eta)
fi

# Hand the script list to parallel. {#} is the job's serial number
# (used to name the log file); {} is the script path. The harness
# already runs as root (checked above), so each child invokes
# bpfman-shell directly with no further elevation; the bin dir is
# prepended to PATH so any helper sub-invocations of bpfman-shell
# from inside a script resolve.
#
# Default halt policy is `never`: keep going through failures so
# the summary can attribute every one. -F/--fail-fast swaps to
# `now,fail=1`, which kills running jobs and stops scheduling new
# ones the moment the first failure lands -- useful when you are
# already triaging a flake and a second instance would only add
# noise.
halt_arg=(--halt never)
if [[ "$fail_fast" -eq 1 ]]; then
    halt_arg=(--halt "now,fail=1")
fi
set +e
printf '%s\n' "${expanded[@]}" | parallel \
    "${progress_args[@]}" \
    --joblog "$joblog" \
    "${halt_arg[@]}" \
    -j "$jobs" \
    "${parallel_passthrough[@]}" \
    "cd \"$e2e_dir\" && PATH=\"$bin_dir:\$PATH\" \"$shell_bin\" {} > \"$logdir/job-{#}.log\" 2>&1"
parallel_rc=$?
set -e

# Parse the joblog for failed entries. The joblog columns we care
# about are Seq, Exitval, and Signal; the Command column is the
# wrapped shell pipeline, not a clean script path, so we recover
# the script identity by indexing into the in-memory scripts[]
# array via Seq -- the same array we fed to parallel, in the
# same order. Under -F/--fail-fast the trigger failure is what
# the user wants to see; parallel's SIGTERM of the in-flight
# siblings is collateral and would only add noise to the summary,
# so we suppress rows where the kernel signal column says the
# job was killed (Signal != 0). Without -F that filter is off,
# because a script that genuinely died on SIGSEGV is a real
# failure worth surfacing.
get_col() {
    awk -v col="$1" 'NR==1 { for (i=1;i<=NF;i++) if ($i==col) { print i; exit } }' "$joblog"
}
seq_col=$(get_col Seq)
exit_col=$(get_col Exitval)
signal_col=$(get_col Signal)

if [[ -z "$seq_col" || -z "$exit_col" || -z "$signal_col" ]]; then
    echo "error: could not parse joblog columns (Seq/Exitval/Signal)" >&2
    exit 1
fi

mapfile -t failures < <(awk -v sc="$seq_col" -v ec="$exit_col" -v sigc="$signal_col" -v ff="$fail_fast" '
    NR > 1 && $ec != 0 {
        if (ff == 1 && $sigc != 0) next
        print $sc "\t" $ec
    }
' "$joblog")

if [[ ${#failures[@]} -eq 0 ]]; then
    echo "all ${#expanded[@]} jobs passed"
    exit 0
fi

echo
echo "${#failures[@]} of ${#expanded[@]} jobs failed:"
for row in "${failures[@]}"; do
    seq=$(echo "$row" | cut -f1)
    exit_code=$(echo "$row" | cut -f2)
    # parallel numbers Seq from 1; expanded[] is zero-indexed.
    script="${expanded[$((seq - 1))]}"
    echo "  - $script (exit $exit_code, job #$seq)"
    echo "      log: $logdir/job-${seq}.log"
done

if [[ $parallel_rc -eq 0 ]]; then
    parallel_rc=2
fi
exit "$parallel_rc"
