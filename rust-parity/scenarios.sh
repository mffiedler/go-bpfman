#!/usr/bin/env bash
# Captures the error / idempotency / flag / format scenarios to
# transcripts under rust-parity/outputs/. Sourced after harness.sh.
capq() { local T="$1"; shift; echo "\$ $*" >>"$T"; CAP_OUT="$("$@" 2>&1)"; CAP_RC=$?; { printf '%s\n' "$CAP_OUT"; echo "[exit $CAP_RC]"; echo; } >>"$T"; }
LIBC=$(ldd "$(command -v cat)" | awk '/libc\.so/{print $3; exit}')
XDP=e2e/testdata/bpf/xdp_pass.bpf.o; IF=bpfmanpar0
mkiface(){ sudo ip link del "$IF" >/dev/null 2>&1; sudo ip link add "$IF" type veth peer name "${IF}p"; sudo ip link set "$IF" up; sudo ip link set "${IF}p" up; }
rmiface(){ sudo ip link del "$IF" >/dev/null 2>&1; }
gun(){ for p in $(gobpf program list -o json 2>/dev/null|jq -r '.programs[].program_id'); do gobpf program unload "$p" >/dev/null 2>&1; done; }
run(){ for p in $(rsbpf list programs 2>/dev/null|awk 'NR>1&&$1~/^[0-9]+$/{print $1}'); do rsbpf unload "$p" >/dev/null 2>&1; done; }

scen_xdp_section() {
  GT="$OUT/xdp-section-format.go.out"; :>"$GT"; RT="$OUT/xdp-section-format.rust.out"; :>"$RT"
  capq "$GT" gobpf program load file e2e/testdata/bpf/xdp_counter.bpf.o --programs xdp:xdp_stats -o json
  local gp; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id'); [ -n "$gp" ] && [ "$gp" != null ] && gobpf program unload "$gp" >/dev/null 2>&1
  capq "$RT" rsbpf load file --path e2e/testdata/bpf/xdp_counter.bpf.o --programs xdp:xdp_stats
  echo "  xdp-section: go-load-exit=ok rust rejects section -> see transcripts"
}
scen_errors() {
  GT="$OUT/errors.go.out"; :>"$GT"; RT="$OUT/errors.rust.out"; :>"$RT"; mkiface
  local gp rp; gp=$(gobpf program load file "$XDP" --programs xdp:pass -o json 2>/dev/null|jq -r '.programs[0].record.program_id'); rp=$(rsbpf load file --path "$XDP" --programs xdp:pass 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$GT" gobpf link attach tc "$gp" "$IF" ingress --priority 100 -o json     # wrong-kind
  capq "$RT" rsbpf attach "$rp" tc --direction ingress --iface "$IF" --priority 100
  gobpf program unload "$gp" >/dev/null 2>&1; rsbpf unload "$rp" >/dev/null 2>&1
  capq "$GT" gobpf program load file /no/such.o --programs xdp:pass -o json       # missing object
  capq "$RT" rsbpf load file --path /no/such.o --programs xdp:pass
  capq "$GT" gobpf program load file "$XDP" --programs garbage -o json            # malformed
  capq "$RT" rsbpf load file --path "$XDP" --programs garbage
  capq "$GT" gobpf program get 99999999 -o json                                   # bad id
  capq "$RT" rsbpf get program 99999999
  rmiface
}
scen_idempotency() {
  GT="$OUT/idempotency.go.out"; :>"$GT"; RT="$OUT/idempotency.rust.out"; :>"$RT"; mkiface
  local gp gl rp rl
  gp=$(gobpf program load file "$XDP" --programs xdp:pass -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  gl=$(gobpf link attach xdp "$gp" "$IF" --priority 100 -o json 2>/dev/null|jq -r '.record.id')
  capq "$GT" gobpf link detach "$gl"; capq "$GT" gobpf link detach "$gl"
  capq "$GT" gobpf program unload "$gp"; capq "$GT" gobpf program unload "$gp"
  rp=$(rsbpf load file --path "$XDP" --programs xdp:pass 2>/dev/null|awk '/Program ID:/{print $NF}')
  rl=$(rsbpf attach "$rp" xdp --iface "$IF" --priority 100 2>/dev/null|awk '/Link ID:/{print $NF}')
  capq "$RT" rsbpf detach "$rl"; capq "$RT" rsbpf detach "$rl"
  capq "$RT" rsbpf unload "$rp"; capq "$RT" rsbpf unload "$rp"
  rmiface
}
scen_priority_zero() {
  GT="$OUT/priority-zero.go.out"; :>"$GT"; RT="$OUT/priority-zero.rust.out"; :>"$RT"; mkiface
  local gp gl rp rl
  gp=$(gobpf program load file "$XDP" --programs xdp:pass -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach xdp "$gp" "$IF" --priority 0 -o json; gl=$(goj "$CAP_OUT" '.record.id')
  capq "$GT" gobpf link get "$gl" -o json
  gobpf link detach "$gl" >/dev/null 2>&1; gobpf program unload "$gp" >/dev/null 2>&1
  rp=$(rsbpf load file --path "$XDP" --programs xdp:pass 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" xdp --iface "$IF" --priority 0; rl=$(rsf "$CAP_OUT" 'Link ID')
  capq "$RT" rsbpf get link "$rl"
  rsbpf detach "$rl" >/dev/null 2>&1; rsbpf unload "$rp" >/dev/null 2>&1
  rmiface
}
scen_global() {
  GT="$OUT/global.go.out"; :>"$GT"; RT="$OUT/global.rust.out"; :>"$RT"
  local O=e2e/testdata/bpf/tc_exact.bpf.o gp
  capq "$GT" gobpf program load file "$O" --programs tc:stats -g weight=0000000000000005 -o json; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id'); [ -n "$gp" ]&&[ "$gp" != null ]&&gobpf program unload "$gp" >/dev/null 2>&1
  capq "$GT" gobpf program load file "$O" --programs tc:stats -g weight=0x0000000000000005 -o json; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id'); [ -n "$gp" ]&&[ "$gp" != null ]&&gobpf program unload "$gp" >/dev/null 2>&1
  capq "$GT" gobpf program load file "$O" --programs tc:stats -g boguskey=01 -o json; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id'); [ -n "$gp" ]&&[ "$gp" != null ]&&gobpf program unload "$gp" >/dev/null 2>&1
  capq "$RT" rsbpf load file --path "$O" --programs tc:stats -g weight=0000000000000005; local rp; rp=$(rsf "$CAP_OUT" 'Program ID'); [ -n "$rp" ]&&rsbpf unload "$rp" >/dev/null 2>&1
  capq "$RT" rsbpf load file --path "$O" --programs tc:stats -g weight=0x0000000000000005
  capq "$RT" rsbpf load file --path "$O" --programs tc:stats -g boguskey=01; rp=$(rsf "$CAP_OUT" 'Program ID'); [ -n "$rp" ]&&rsbpf unload "$rp" >/dev/null 2>&1
  run
}
scen_application() {
  GT="$OUT/application.go.out"; :>"$GT"; RT="$OUT/application.rust.out"; :>"$RT"
  local O=e2e/testdata/bpf/tcx_counter.bpf.o K=e2e/testdata/bpf/kprobe_counter.bpf.o
  gobpf program load file "$O" --programs tcx:tcx_stats --application appA -o json >/dev/null 2>&1
  gobpf program load file "$K" --programs kprobe:kprobe_counter --application appB -o json >/dev/null 2>&1
  capq "$GT" gobpf program list --application appA -o json
  capq "$GT" gobpf program list -o json
  gun
  rsbpf load file --path "$O" --programs tcx:tcx_stats --application appA >/dev/null 2>&1
  rsbpf load file --path "$K" --programs kprobe:kprobe_counter --application appB >/dev/null 2>&1
  capq "$RT" rsbpf list programs --application appA
  capq "$RT" rsbpf list programs
  run
}
scen_map_owner() {
  GT="$OUT/map-owner-id.go.out"; :>"$GT"; RT="$OUT/map-owner-id.rust.out"; :>"$RT"
  local O=e2e/testdata/bpf/tcx_counter.bpf.o oid sid
  capq "$GT" gobpf program load file "$O" --programs tcx:tcx_stats -o json; oid=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  capq "$GT" gobpf program load file "$O" --programs tcx:tcx_stats --map-owner-id "$oid" -o json; sid=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  capq "$GT" sudo bpftool prog show id "$oid"; capq "$GT" sudo bpftool prog show id "$sid"
  echo "  GO  owner/sharer kernel map_ids -> see map-owner-id.go.out (bpftool prog show)"
  gun
  capq "$RT" rsbpf load file --path "$O" --programs tcx:tcx_stats; oid=$(rsf "$CAP_OUT" 'Program ID')
  capq "$RT" rsbpf load file --path "$O" --programs tcx:tcx_stats --map-owner-id "$oid"; sid=$(rsf "$CAP_OUT" 'Program ID')
  capq "$RT" sudo bpftool prog show id "$oid"; capq "$RT" sudo bpftool prog show id "$sid"
  echo "  RUST owner/sharer kernel map_ids -> see map-owner-id.rust.out (bpftool prog show)"
  run
}
scen_flags() {
  GT="$OUT/flags.go.out"; :>"$GT"; RT="$OUT/flags.rust.out"; :>"$RT"; mkiface
  local gp rp
  # tcx egress
  gp=$(gobpf program load file e2e/testdata/bpf/tcx_counter.bpf.o --programs tcx:tcx_stats -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach tcx "$gp" "$IF" egress --priority 100 -o json; gun
  rp=$(rsbpf load file --path e2e/testdata/bpf/tcx_counter.bpf.o --programs tcx:tcx_stats 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" tcx --direction egress --iface "$IF" --priority 100; run
  # kprobe offset
  gp=$(gobpf program load file e2e/testdata/bpf/kprobe_counter.bpf.o --programs kprobe:kprobe_counter -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach kprobe "$gp" vfs_read --offset 0 -o json; gun
  rp=$(rsbpf load file --path e2e/testdata/bpf/kprobe_counter.bpf.o --programs kprobe:kprobe_counter 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" kprobe --fn-name vfs_read --offset 0; run
  # uprobe pid + offset-only + container-pid
  gp=$(gobpf program load file e2e/testdata/bpf/uprobe_counter.bpf.o --programs uprobe:uprobe_counter -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach uprobe "$gp" "$LIBC" --fn-name malloc --pid 1 -o json; gun
  gp=$(gobpf program load file e2e/testdata/bpf/uprobe_counter.bpf.o --programs uprobe:uprobe_counter -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach uprobe "$gp" "$LIBC" --offset 4096 -o json; gun
  gp=$(gobpf program load file e2e/testdata/bpf/uprobe_counter.bpf.o --programs uprobe:uprobe_counter -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach uprobe "$gp" "$LIBC" --fn-name malloc --container-pid 1 -o json; gun
  rp=$(rsbpf load file --path e2e/testdata/bpf/uprobe_counter.bpf.o --programs uprobe:uprobe_counter 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" uprobe --target "$LIBC" --fn-name malloc --pid 1; run
  rp=$(rsbpf load file --path e2e/testdata/bpf/uprobe_counter.bpf.o --programs uprobe:uprobe_counter 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" uprobe --target "$LIBC" --offset 4096; run
  rp=$(rsbpf load file --path e2e/testdata/bpf/uprobe_counter.bpf.o --programs uprobe:uprobe_counter 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" uprobe --target "$LIBC" --fn-name malloc --container-pid 1; run
  rp=$(rsbpf load file --path e2e/testdata/bpf/kprobe_counter.bpf.o --programs kprobe:kprobe_counter 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" kprobe --fn-name vfs_read --container-pid 1; run
  rmiface
}

# Image load (needs registry reachability). Loads a public bytecode
# image on both impls, captures the load + get, then unloads.
scen_image() {
  local IMG=quay.io/bpfman-bytecode/xdp_pass:latest
  GT="$OUT/image-load.go.out"; :>"$GT"; RT="$OUT/image-load.rust.out"; :>"$RT"
  capq "$GT" gobpf program load image "$IMG" --programs xdp:pass --pull-policy Always -o json
  local gp; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  capq "$GT" gobpf program get "$gp" -o json
  [ -n "$gp" ] && [ "$gp" != null ] && gobpf program unload "$gp" >/dev/null 2>&1
  capq "$RT" rsbpf load image --image-url "$IMG" --programs xdp:pass --pull-policy Always
  local rp; rp=$(rsf "$CAP_OUT" 'Program ID')
  capq "$RT" rsbpf get program "$rp"
  [ -n "$rp" ] && rsbpf unload "$rp" >/dev/null 2>&1
}
