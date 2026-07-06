#!/usr/bin/env bash
# Regenerate every parity transcript under rust-parity/outputs/.
#
# Run from anywhere, with bash, after the binaries are in place (see
# README.md "Setup"). Requires: sudo (passwordless), jq, ip, bpftool,
# and the compiled testdata objects under e2e/testdata/bpf/.
#
# NOT `set -e`: scenarios deliberately exercise failing commands and
# record the outcome rather than aborting.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$here/harness.sh"
source "$here/baselines.sh"
source "$here/ordering.sh"
source "$here/slotreuse.sh"
source "$here/scenarios.sh"
source "$here/extra.sh"

cd "$REPO" || exit 1   # object paths are repo-relative

say() { printf '\n========== %s ==========\n' "$*"; }

say "baseline lifecycles (10 program types)"
baselines_all

say "priority ordering chains"
chain_disp ordering-xdp xdp        xdp e2e/testdata/bpf/xdp_pass.bpf.o   xdp:pass      50 10 40 20 30
chain_disp ordering-tc  tc-ingress tc  e2e/testdata/bpf/tc_counter.bpf.o tc:stats      50 10 40 20 30
chain_tcx  ordering-tcx                 e2e/testdata/bpf/tcx_counter.bpf.o tcx:tcx_stats 50 10 40 20 30

say "slot reuse + exhaustion"
slot_reuse   xdp        xdp e2e/testdata/bpf/xdp_pass.bpf.o   xdp:pass
slot_reuse   tc-ingress tc  e2e/testdata/bpf/tc_counter.bpf.o tc:stats
slot_exhaust xdp        xdp e2e/testdata/bpf/xdp_pass.bpf.o   xdp:pass
slot_exhaust tc-ingress tc  e2e/testdata/bpf/tc_counter.bpf.o tc:stats

say "tie-break / detach-middle / multi-interface / netns"
scen_tiebreak
scen_detach_middle
scen_multi_iface
scen_netns xdp e2e/testdata/bpf/xdp_pass.bpf.o   xdp:pass
scen_netns tc  e2e/testdata/bpf/tc_counter.bpf.o tc:stats
scen_netns tcx e2e/testdata/bpf/tcx_counter.bpf.o tcx:tcx_stats
scen_ns_deleted_detach xdp e2e/testdata/bpf/xdp_pass.bpf.o    xdp:pass
scen_ns_deleted_detach tc  e2e/testdata/bpf/tc_counter.bpf.o  tc:stats
scen_ns_deleted_detach tcx e2e/testdata/bpf/tcx_counter.bpf.o tcx:tcx_stats

say "errors / idempotency / flags / formats"
scen_xdp_section
scen_errors
scen_idempotency
scen_priority_zero
scen_global
scen_application
scen_map_owner
scen_flags
scen_image   # needs registry reachability (quay.io)

say "done -- transcripts under $OUT"
echo "leftover go programs: $(gobpf program list -o json 2>/dev/null | jq '.programs|length')"
echo "leftover rust programs: $(rsbpf list programs 2>/dev/null | awk 'NR>1&&$1~/^[0-9]+$/' | wc -l)"
