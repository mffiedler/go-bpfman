#!/usr/bin/env bash
# Capture a normalised observation for each behavioural parity case, for
# both implementations, under docs/parity/outputs/behaviour/. Unlike the
# kernel footprints, these observations are the semantically comparable
# result of a command sequence -- exit codes, chain positions, kernel
# map sharing as bpftool sees it, ordering determinism over repeated
# runs -- captured as small JSON objects. The same Go comparator diffs
# Go's observation against Rust's and judges it against cases.yaml.
#
# Where the observation can be sourced from the kernel (bpftool) rather
# than the tool's own report, it is, and the field is named accordingly.
set -uo pipefail
REPO="${PARITY_REPO:-$HOME/src/github.com/frobware/go-bpfman}"
GO=(sudo "$REPO/bin/go-bpfman" --runtime-dir /run/bpfman-parity-go)
RS=(sudo "$REPO/bin/bpfman-rs")
OUT="$REPO/docs/parity/outputs/behaviour"
mkdir -p "$OUT"
LIBC=$(ldd "$(command -v cat)" | awk '/libc\.so/{print $3; exit}')
XDP=e2e/testdata/bpf/xdp_pass.bpf.o
IF=bpfmanpar0

cd "$REPO" || exit 1
# Always detach links before unloading and before any interface is torn
# down: deleting a veth out from under a live dispatcher link severs it
# and wedges the store.
gclean(){
  for l in $("${GO[@]}" link list -o json 2>/dev/null|jq -r '.links[].id'); do "${GO[@]}" link detach "$l" >/dev/null 2>&1; done
  for p in $("${GO[@]}" program list -o json 2>/dev/null|jq -r '.programs[].program_id'); do "${GO[@]}" program unload "$p" >/dev/null 2>&1; done
}
rclean(){
  for l in $("${RS[@]}" list links 2>/dev/null|awk 'NR>1 && $2 ~ /^[0-9]+$/ {print $2}'); do "${RS[@]}" detach "$l" >/dev/null 2>&1; done
  for p in $("${RS[@]}" list programs 2>/dev/null|awk 'NR>1&&$1~/^[0-9]+$/{print $1}'); do "${RS[@]}" unload "$p" >/dev/null 2>&1; done
}
ifup(){ sudo ip link del "$IF" >/dev/null 2>&1; sudo ip link add "$IF" type veth peer name "${IF}p"; sudo ip link set "$IF" up; sudo ip link set "${IF}p" up; }
ifdown(){ sudo ip link del "$IF" >/dev/null 2>&1; }
ec(){ "$@" >/dev/null 2>&1; echo $?; }   # exit code of a command
gload(){ "${GO[@]}" program load file "$1" --programs "$2" -o json 2>/dev/null|jq -r '.programs[0].record.program_id'; }
rload(){ "${RS[@]}" load file --path "$1" --programs "$2" 2>/dev/null|awk '/Program ID:/{print $NF}'; }
gmapid(){ "${GO[@]}" program get "$1" -o json 2>/dev/null|jq -r '.status.maps[0].id'; }
rmapid(){ sudo bpftool prog show id "$1" -j 2>/dev/null|jq -r '.map_ids[0]'; }

# --- error class + exit code: both fail, exit codes may differ ---
errcase(){ # NAME GO_EC RUST_EC
  jq -n --argjson ge "$2" --argjson re "$3" '{failed:($ge!=0), exit:$ge}' > "$OUT/$1.go.json"
  jq -n --argjson ge "$2" --argjson re "$3" '{failed:($re!=0), exit:$re}' > "$OUT/$1.rust.json"
}
cap_errors(){
  ifup
  local gp rp
  gp=$(gload "$XDP" xdp:pass); rp=$(rload "$XDP" xdp:pass)
  errcase errors-wrong-kind \
    "$(ec "${GO[@]}" link attach tc "$gp" "$IF" ingress --priority 100)" \
    "$(ec "${RS[@]}" attach "$rp" tc --direction ingress --iface "$IF" --priority 100)"
  gclean; rclean
  errcase errors-missing-object \
    "$(ec "${GO[@]}" program load file /no/such.o --programs xdp:pass)" \
    "$(ec "${RS[@]}" load file --path /no/such.o --programs xdp:pass)"
  errcase errors-malformed \
    "$(ec "${GO[@]}" program load file "$XDP" --programs garbage)" \
    "$(ec "${RS[@]}" load file --path "$XDP" --programs garbage)"
  errcase errors-bad-id \
    "$(ec "${GO[@]}" program get 99999999)" \
    "$(ec "${RS[@]}" get program 99999999)"
  ifdown
}

# --- section format: Go loads SEC("xdp/<name>"), Rust rejects ---
cap_section(){
  local o=e2e/testdata/bpf/xdp_counter.bpf.o
  local g r; g=$(ec "${GO[@]}" program load file "$o" --programs xdp:xdp_stats -o json); r=$(ec "${RS[@]}" load file --path "$o" --programs xdp:xdp_stats)
  gclean; rclean
  jq -n --argjson e "$g" '{load_exit:$e, loaded:($e==0)}' > "$OUT/section-format.go.json"
  jq -n --argjson e "$r" '{load_exit:$e, loaded:($e==0)}' > "$OUT/section-format.rust.json"
}

# --- global 0x prefix: Go accepts both forms, Rust rejects 0x ---
cap_global(){
  local o=e2e/testdata/bpf/tc_exact.bpf.o
  local gb gp rb rp
  gb=$(ec "${GO[@]}" program load file "$o" --programs tc:stats -g weight=0000000000000005 -o json); gclean
  gp=$(ec "${GO[@]}" program load file "$o" --programs tc:stats -g weight=0x0000000000000005 -o json); gclean
  rb=$(ec "${RS[@]}" load file --path "$o" --programs tc:stats -g weight=0000000000000005); rclean
  rp=$(ec "${RS[@]}" load file --path "$o" --programs tc:stats -g weight=0x0000000000000005); rclean
  jq -n --argjson b "$gb" --argjson p "$gp" '{bare_hex_loaded:($b==0), prefixed_0x_loaded:($p==0)}' > "$OUT/global-prefix.go.json"
  jq -n --argjson b "$rb" --argjson p "$rp" '{bare_hex_loaded:($b==0), prefixed_0x_loaded:($p==0)}' > "$OUT/global-prefix.rust.json"
}

# --- map-owner-id: do owner and sharer share one kernel map? (bpftool) ---
cap_map_owner(){
  local o=e2e/testdata/bpf/tcx_counter.bpf.o
  local oid sid om sm
  oid=$(gload "$o" tcx:tcx_stats); om=$(gmapid "$oid")
  sid=$("${GO[@]}" program load file "$o" --programs tcx:tcx_stats --map-owner-id "$oid" -o json 2>/dev/null|jq -r '.programs[0].record.program_id'); sm=$(gmapid "$sid")
  jq -n --arg sh "$([ "$om" = "$sm" ] && echo true || echo false)" '{shares_kernel_map:($sh=="true")}' > "$OUT/map-owner.go.json"; gclean
  oid=$(rload "$o" tcx:tcx_stats); om=$(rmapid "$oid")
  sid=$("${RS[@]}" load file --path "$o" --programs tcx:tcx_stats --map-owner-id "$oid" 2>/dev/null|awk '/Program ID:/{print $NF}'); sm=$(rmapid "$sid")
  jq -n --arg sh "$([ "$om" = "$sm" ] && echo true || echo false)" '{shares_kernel_map:($sh=="true")}' > "$OUT/map-owner.rust.json"; rclean
}

# --- container-pid: Go accepts + attaches, Rust rejects ---
cap_container_pid(){
  local o=e2e/testdata/bpf/uprobe_counter.bpf.o gp rp g r
  gp=$(gload "$o" uprobe:uprobe_counter)
  g=$(ec "${GO[@]}" link attach uprobe "$gp" "$LIBC" --fn-name malloc --container-pid 1); gclean
  rp=$(rload "$o" uprobe:uprobe_counter)
  r=$(ec "${RS[@]}" attach "$rp" uprobe --target "$LIBC" --fn-name malloc --container-pid 1); rclean
  jq -n --argjson e "$g" '{attach_exit:$e, accepted:($e==0)}' > "$OUT/container-pid.go.json"
  jq -n --argjson e "$r" '{attach_exit:$e, accepted:($e==0)}' > "$OUT/container-pid.rust.json"
}

# --- idempotency: second detach / second unload both fail ---
cap_idempotency(){
  ifup
  local gp gl d1 d2 u1 u2
  gp=$(gload "$XDP" xdp:pass); gl=$("${GO[@]}" link attach xdp "$gp" "$IF" --priority 100 -o json 2>/dev/null|jq -r '.record.id')
  d1=$(ec "${GO[@]}" link detach "$gl"); d2=$(ec "${GO[@]}" link detach "$gl")
  u1=$(ec "${GO[@]}" program unload "$gp"); u2=$(ec "${GO[@]}" program unload "$gp")
  jq -n --argjson d1 "$d1" --argjson d2 "$d2" --argjson u1 "$u1" --argjson u2 "$u2" \
    '{detach_first_ok:($d1==0), detach_second_fails:($d2!=0), unload_first_ok:($u1==0), unload_second_fails:($u2!=0)}' > "$OUT/idempotency.go.json"
  local rp rl
  rp=$(rload "$XDP" xdp:pass); rl=$("${RS[@]}" attach "$rp" xdp --iface "$IF" --priority 100 2>/dev/null|awk '/Link ID:/{print $NF}')
  d1=$(ec "${RS[@]}" detach "$rl"); d2=$(ec "${RS[@]}" detach "$rl")
  u1=$(ec "${RS[@]}" unload "$rp"); u2=$(ec "${RS[@]}" unload "$rp")
  jq -n --argjson d1 "$d1" --argjson d2 "$d2" --argjson u1 "$u1" --argjson u2 "$u2" \
    '{detach_first_ok:($d1==0), detach_second_fails:($d2!=0), unload_first_ok:($u1==0), unload_second_fails:($u2!=0)}' > "$OUT/idempotency.rust.json"
  ifdown
}

# --- priority 0: accepted, stored as priority 0 / position 0 ---
cap_priority_zero(){
  ifup
  local gp gl
  gp=$(gload "$XDP" xdp:pass); gl=$("${GO[@]}" link attach xdp "$gp" "$IF" --priority 0 -o json 2>/dev/null)
  jq -n --argjson p "$(echo "$gl"|jq '.record.details.priority')" --argjson pos "$(echo "$gl"|jq '.record.details.position')" '{priority:$p, position:$pos}' > "$OUT/priority-zero.go.json"; gclean
  local rp out
  rp=$(rload "$XDP" xdp:pass); out=$("${RS[@]}" attach "$rp" xdp --iface "$IF" --priority 0 2>/dev/null)
  jq -n --argjson p "$(echo "$out"|awk '/Priority:/{print $NF}')" --argjson pos "$(echo "$out"|awk '/Position:/{print $NF}')" '{priority:$p, position:$pos}' > "$OUT/priority-zero.rust.json"; rclean
  ifdown
}

# --- slot exhaustion: 10 fill, 11th rejected ---
cap_slot_exhaustion(){
  ifup
  local gp e11
  gp=$(gload "$XDP" xdp:pass)
  for p in 100 200 300 400 500 600 700 800 900 1000; do "${GO[@]}" link attach xdp "$gp" "$IF" --priority "$p" -o json >/dev/null 2>&1; done
  e11=$(ec "${GO[@]}" link attach xdp "$gp" "$IF" --priority 150); gclean
  jq -n --argjson e "$e11" '{eleventh_rejected:($e!=0)}' > "$OUT/slot-exhaustion.go.json"
  local rp
  rp=$(rload "$XDP" xdp:pass)
  for p in 100 200 300 400 500 600 700 800 900 1000; do "${RS[@]}" attach "$rp" xdp --iface "$IF" --priority "$p" >/dev/null 2>&1; done
  e11=$(ec "${RS[@]}" attach "$rp" xdp --iface "$IF" --priority 150); rclean
  jq -n --argjson e "$e11" '{eleventh_rejected:($e!=0)}' > "$OUT/slot-exhaustion.rust.json"
  ifdown
}

# --- priority ordering: final positions follow priority rank ---
# Attach all, THEN read each link's final position (positions only settle
# once the whole chain is in place).
cap_ordering(){
  ifup
  local gp; gp=$(gload "$XDP" xdp:pass)
  local gl=() l
  for p in 50 10 40 20 30; do gl+=("$("${GO[@]}" link attach xdp "$gp" "$IF" --priority "$p" -o json 2>/dev/null|jq -r '.record.id')"); done
  local pos=()
  for l in "${gl[@]}"; do pos+=("$("${GO[@]}" link get "$l" -o json 2>/dev/null|jq -r '.record.details.position')"); done
  printf '%s\n' "${pos[@]}" | jq -s '{positions:.}' > "$OUT/ordering-xdp.go.json"; gclean; ifdown; ifup
  local rp; rp=$(rload "$XDP" xdp:pass); local rl=()
  for p in 50 10 40 20 30; do rl+=("$("${RS[@]}" attach "$rp" xdp --iface "$IF" --priority "$p" 2>/dev/null|awk '/Link ID:/{print $NF}')"); done
  local rpos=()
  for l in "${rl[@]}"; do rpos+=("$("${RS[@]}" get link "$l" 2>/dev/null|awk '/Position:/{print $NF}')"); done
  printf '%s\n' "${rpos[@]}" | jq -s '{positions:.}' > "$OUT/ordering-xdp.rust.json"; rclean; ifdown
}

# --- tie-break determinism over repeated runs ---
cap_tiebreak(){
  local runs=6
  tb(){ # impl -> prints "a,b,c" chain order (attach# at pos0,1,2)
    ifup
    local prog l ids=()
    if [ "$1" = go ]; then prog=$(gload "$XDP" xdp:pass)
      for i in 1 2 3; do ids+=("$("${GO[@]}" link attach xdp "$prog" "$IF" --priority 100 -o json 2>/dev/null|jq -r '.record.id')"); done
      local map=(); local n=0
      for l in "${ids[@]}"; do n=$((n+1)); local p; p=$("${GO[@]}" link get "$l" -o json 2>/dev/null|jq -r '.record.details.position'); map[$p]=$n; done
      gclean
    else prog=$(rload "$XDP" xdp:pass)
      for i in 1 2 3; do ids+=("$("${RS[@]}" attach "$prog" xdp --iface "$IF" --priority 100 2>/dev/null|awk '/Link ID:/{print $NF}')"); done
      local map=(); local n=0
      for l in "${ids[@]}"; do n=$((n+1)); local p; p=$("${RS[@]}" get link "$l" 2>/dev/null|awk '/Position:/{print $NF}'); map[$p]=$n; done
      rclean
    fi
    ifdown
    echo "${map[0]},${map[1]},${map[2]}"
  }
  for impl in go rust; do
    local seen; seen=$(for r in $(seq 1 $runs); do tb "$impl"; done | sort -u | wc -l | tr -d ' ')
    jq -n --argjson n "$seen" '{deterministic:($n==1), distinct_orders:$n}' > "$OUT/tiebreak.$impl.json"
  done
}

capture_behaviour(){
  cap_errors; echo "  errors"
  cap_section; echo "  section-format"
  cap_global; echo "  global-prefix"
  cap_map_owner; echo "  map-owner"
  cap_container_pid; echo "  container-pid"
  cap_idempotency; echo "  idempotency"
  cap_priority_zero; echo "  priority-zero"
  cap_slot_exhaustion; echo "  slot-exhaustion"
  cap_ordering; echo "  ordering-xdp"
  cap_tiebreak; echo "  tiebreak"
}
