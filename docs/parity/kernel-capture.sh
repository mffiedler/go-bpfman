#!/usr/bin/env bash
# Capture the kernel + bpffs footprint of each parity case, as seen by
# bpftool (the neutral juror), for both implementations. For a given
# program type we load + attach with each tool, ask the tool only which
# kernel program id it created, then let bpftool describe that program,
# its maps, and its link. The captured footprint is normalised (ids,
# timestamps, addresses, ifindex dropped) so the Go comparator can diff
# Go's kernel footprint against Rust's and compute a verdict.
#
# Output: docs/parity/outputs/kernel/<case>.<impl>.json
set -uo pipefail
REPO="${PARITY_REPO:-$HOME/src/github.com/frobware/go-bpfman}"
GO=(sudo "$REPO/bin/go-bpfman" --runtime-dir /run/bpfman-parity-go)
RS=(sudo "$REPO/bin/bpfman-rs")
OUT="$REPO/docs/parity/outputs/kernel"
mkdir -p "$OUT"
LIBC=$(ldd "$(command -v cat)" | awk '/libc\.so/{print $3; exit}')
IF=bpfmanpar0

clean_go(){ for p in $("${GO[@]}" program list -o json 2>/dev/null|jq -r '.programs[].program_id'); do "${GO[@]}" program unload "$p" >/dev/null 2>&1; done; }
clean_rs(){ for p in $("${RS[@]}" list programs 2>/dev/null|awk 'NR>1&&$1~/^[0-9]+$/{print $1}'); do "${RS[@]}" unload "$p" >/dev/null 2>&1; done; }
veth_up(){ sudo ip link del "$IF" >/dev/null 2>&1; sudo ip link add "$IF" type veth peer name "${IF}p"; sudo ip link set "$IF" up; sudo ip link set "${IF}p" up; }
veth_down(){ sudo ip link del "$IF" >/dev/null 2>&1; }

# footprint KERNEL_PROG_ID OUTFILE -- bpftool's normalised view.
footprint(){
  local K="$1" out="$2"
  local prog maps link
  prog=$(sudo bpftool prog show id "$K" -j 2>/dev/null | jq '{type,name,tag,gpl_compatible}')
  maps=$(for m in $(sudo bpftool prog show id "$K" -j 2>/dev/null | jq -r '.map_ids[]?'); do
           sudo bpftool map show id "$m" -j 2>/dev/null | jq '{type,name,bytes_key,bytes_value,max_entries,flags}'
         done | jq -s 'sort_by(.name)')
  # the link(s) whose prog is this program; drop unstable fields.
  # Drop every id/address-like field so only the semantic shape remains:
  # link id, prog id, kernel symbol address, perf cookie/missed counters,
  # ifindex, netns inode, and the freplace target object/btf ids (which
  # are the dispatcher's internal ids and differ as ids, not semantics).
  link=$(sudo bpftool link show -j 2>/dev/null \
           | jq "[.[]|select(.prog_id==$K)] | map(del(.id,.prog_id,.addr,.cookie,.missed,.ifindex,.netns_ino,.target_obj_id,.target_btf_id)) | sort_by(.type)")
  jq -n --argjson p "${prog:-null}" --argjson m "${maps:-[]}" --argjson l "${link:-[]}" \
     '{program:$p, maps:$m, link:$l}' > "$out"
}

# go_kid <load-json> ; rust_kid <load-text>  -- the kernel program id.
go_kid(){ jq -r '.programs[0].status.kernel.id'; }
rust_kid(){ awk '/Program ID:/{print $NF; exit}'; }

capture(){ # CASE TYPE OBJ SPEC
  local case=$1 type=$2 obj=$3 spec=$4 needs_if=0
  case "$type" in xdp|tc|tcx) needs_if=1;; esac
  clean_go; clean_rs
  # GO
  [ $needs_if = 1 ] && veth_up
  local K
  K=$("${GO[@]}" program load file "$obj" --programs "$spec" -o json 2>/dev/null | go_kid)
  go_attach "$type" "$K"
  footprint "$K" "$OUT/$case.go.json"
  clean_go; [ $needs_if = 1 ] && veth_down
  # RUST
  [ $needs_if = 1 ] && veth_up
  K=$("${RS[@]}" load file --path "$obj" --programs "$spec" 2>/dev/null | rust_kid)
  rust_attach "$type" "$K"
  footprint "$K" "$OUT/$case.rust.json"
  clean_rs; [ $needs_if = 1 ] && veth_down
  echo "  captured $case"
}

go_attach(){ # TYPE KPROG
  case "$1" in
    xdp) "${GO[@]}" link attach xdp "$2" "$IF" --priority 100 -o json >/dev/null 2>&1;;
    tc|tcx) "${GO[@]}" link attach "$1" "$2" "$IF" ingress --priority 100 -o json >/dev/null 2>&1;;
    tracepoint) "${GO[@]}" link attach tracepoint "$2" syscalls/sys_enter_kill -o json >/dev/null 2>&1;;
    kprobe|kretprobe) "${GO[@]}" link attach kprobe "$2" vfs_read -o json >/dev/null 2>&1;;
    uprobe|uretprobe) "${GO[@]}" link attach uprobe "$2" "$LIBC" --fn-name malloc -o json >/dev/null 2>&1;;
    fentry|fexit) "${GO[@]}" link attach "$1" "$2" -o json >/dev/null 2>&1;;
  esac
}
rust_attach(){ # TYPE KPROG
  case "$1" in
    xdp) "${RS[@]}" attach "$2" xdp --iface "$IF" --priority 100 >/dev/null 2>&1;;
    tc|tcx) "${RS[@]}" attach "$2" "$1" --direction ingress --iface "$IF" --priority 100 >/dev/null 2>&1;;
    tracepoint) "${RS[@]}" attach "$2" tracepoint --tracepoint syscalls/sys_enter_kill >/dev/null 2>&1;;
    kprobe|kretprobe) "${RS[@]}" attach "$2" kprobe --fn-name vfs_read >/dev/null 2>&1;;
    uprobe|uretprobe) "${RS[@]}" attach "$2" uprobe --target "$LIBC" --fn-name malloc >/dev/null 2>&1;;
    fentry|fexit) "${RS[@]}" attach "$2" "$1" >/dev/null 2>&1;;
  esac
}

capture_all(){
  capture xdp        xdp        e2e/testdata/bpf/xdp_pass.bpf.o          xdp:pass
  capture tc         tc         e2e/testdata/bpf/tc_counter.bpf.o        tc:stats
  capture tcx        tcx        e2e/testdata/bpf/tcx_counter.bpf.o       tcx:tcx_stats
  capture tracepoint tracepoint e2e/testdata/bpf/tracepoint_counter.bpf.o tracepoint:tracepoint_kill_recorder
  capture kprobe     kprobe     e2e/testdata/bpf/kprobe_counter.bpf.o    kprobe:kprobe_counter
  capture kretprobe  kretprobe  e2e/testdata/bpf/kprobe_counter.bpf.o    kretprobe:kprobe_counter
  capture uprobe     uprobe     e2e/testdata/bpf/uprobe_counter.bpf.o    uprobe:uprobe_counter
  capture uretprobe  uretprobe  e2e/testdata/bpf/uprobe_counter.bpf.o    uretprobe:uprobe_counter
  capture fentry     fentry     e2e/testdata/bpf/fentry_counter.bpf.o    fentry:test_fentry:do_unlinkat
  capture fexit      fexit      e2e/testdata/bpf/fentry_counter.bpf.o    fexit:test_fexit:do_unlinkat
}
