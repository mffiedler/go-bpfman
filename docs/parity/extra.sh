#!/usr/bin/env bash
# Tie-break, detach-from-middle, multi-interface independence and
# --netns scenarios. Sourced after harness.sh.
capq() { local T="$1"; shift; echo "\$ $*" >>"$T"; CAP_OUT="$("$@" 2>&1)"; CAP_RC=$?; { printf '%s\n' "$CAP_OUT"; echo "[exit $CAP_RC]"; echo; } >>"$T"; }
XDP=e2e/testdata/bpf/xdp_pass.bpf.o

scen_tiebreak() { # 3x XDP attach at equal priority 100
  local IF=bpfmanpar9
  GT="$OUT/ordering-tiebreak.go.out"; :>"$GT"; veth_up "$IF"
  local gp gl=(); gp=$(gobpf program load file "$XDP" --programs xdp:pass -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  for i in 1 2 3; do capq "$GT" gobpf link attach xdp "$gp" "$IF" --priority 100 -o json; gl+=("$(goj "$CAP_OUT" '.record.id')"); done
  for l in "${gl[@]}"; do capq "$GT" gobpf link get "$l" -o json; done
  for l in "${gl[@]}"; do capq "$GT" gobpf link detach "$l"; done; capq "$GT" gobpf program unload "$gp"; veth_down "$IF"
  RT="$OUT/ordering-tiebreak.rust.out"; :>"$RT"; veth_up "$IF"
  local rp rl=(); rp=$(rsbpf load file --path "$XDP" --programs xdp:pass 2>/dev/null|awk '/Program ID:/{print $NF}')
  for i in 1 2 3; do capq "$RT" rsbpf attach "$rp" xdp --iface "$IF" --priority 100; rl+=("$(rsf "$CAP_OUT" 'Link ID')"); done
  for l in "${rl[@]}"; do capq "$RT" rsbpf get link "$l"; done
  for l in "${rl[@]}"; do capq "$RT" rsbpf detach "$l"; done; capq "$RT" rsbpf unload "$rp"; veth_down "$IF"
}
scen_detach_middle() { # attach 10,20,30; detach 20; re-read
  local IF=bpfmanpar9
  GT="$OUT/ordering-detach-middle.go.out"; :>"$GT"; veth_up "$IF"
  local gp; gp=$(gobpf program load file "$XDP" --programs xdp:pass -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  declare -A gpl; for p in 10 20 30; do capq "$GT" gobpf link attach xdp "$gp" "$IF" --priority "$p" -o json; gpl[$p]=$(goj "$CAP_OUT" '.record.id'); done
  for p in 10 20 30; do capq "$GT" gobpf link get "${gpl[$p]}" -o json; done
  capq "$GT" gobpf link detach "${gpl[20]}"
  for p in 10 30; do capq "$GT" gobpf link get "${gpl[$p]}" -o json; done
  for p in 10 30; do capq "$GT" gobpf link detach "${gpl[$p]}"; done; capq "$GT" gobpf program unload "$gp"; veth_down "$IF"; unset gpl
  RT="$OUT/ordering-detach-middle.rust.out"; :>"$RT"; veth_up "$IF"
  local rp; rp=$(rsbpf load file --path "$XDP" --programs xdp:pass 2>/dev/null|awk '/Program ID:/{print $NF}')
  declare -A rpl; for p in 10 20 30; do capq "$RT" rsbpf attach "$rp" xdp --iface "$IF" --priority "$p"; rpl[$p]=$(rsf "$CAP_OUT" 'Link ID'); done
  for p in 10 20 30; do capq "$RT" rsbpf get link "${rpl[$p]}"; done
  capq "$RT" rsbpf detach "${rpl[20]}"
  for p in 10 30; do capq "$RT" rsbpf get link "${rpl[$p]}"; done
  for p in 10 30; do capq "$RT" rsbpf detach "${rpl[$p]}"; done; capq "$RT" rsbpf unload "$rp"; veth_down "$IF"; unset rpl
}
scen_multi_iface() {
  local IF1=bpfmanpa IF2=bpfmanpb
  for IFX in "$IF1" "$IF2"; do sudo ip link del "$IFX" >/dev/null 2>&1; sudo ip link add "$IFX" type veth peer name "${IFX}p"; sudo ip link set "$IFX" up; sudo ip link set "${IFX}p" up; done
  GT="$OUT/multi-iface-xdp.go.out"; :>"$GT"
  local gp gl1 gl2; gp=$(gobpf program load file "$XDP" --programs xdp:pass -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  capq "$GT" gobpf link attach xdp "$gp" "$IF1" --priority 100 -o json; gl1=$(goj "$CAP_OUT" '.record.id')
  capq "$GT" gobpf link attach xdp "$gp" "$IF2" --priority 100 -o json; gl2=$(goj "$CAP_OUT" '.record.id')
  capq "$GT" gobpf dispatcher list -o json
  capq "$GT" gobpf link detach "$gl1"; capq "$GT" gobpf dispatcher list -o json; capq "$GT" gobpf link get "$gl2" -o json
  capq "$GT" gobpf link detach "$gl2"; capq "$GT" gobpf program unload "$gp"
  RT="$OUT/multi-iface-xdp.rust.out"; :>"$RT"
  local rp rl1 rl2; rp=$(rsbpf load file --path "$XDP" --programs xdp:pass 2>/dev/null|awk '/Program ID:/{print $NF}')
  capq "$RT" rsbpf attach "$rp" xdp --iface "$IF1" --priority 100; rl1=$(rsf "$CAP_OUT" 'Link ID')
  capq "$RT" rsbpf attach "$rp" xdp --iface "$IF2" --priority 100; rl2=$(rsf "$CAP_OUT" 'Link ID')
  capq "$RT" rsbpf list links; capq "$RT" rsbpf detach "$rl1"; capq "$RT" rsbpf get link "$rl2"; capq "$RT" rsbpf list links
  capq "$RT" rsbpf detach "$rl2"; capq "$RT" rsbpf unload "$rp"
  for IFX in "$IF1" "$IF2"; do sudo ip link del "$IFX" >/dev/null 2>&1; done
}
scen_netns() { # TYPE OBJ SPEC
  local NS=bpfmanns NSPATH=/var/run/netns/bpfmanns NSIF=nsveth0 type=$1 obj=$2 spec=$3 tag="netns-$1"
  netup(){ sudo ip netns del "$NS" >/dev/null 2>&1; sudo ip link del "$NSIF" >/dev/null 2>&1; sudo ip netns add "$NS"; sudo ip link add "$NSIF" type veth peer name "${NSIF}p"; sudo ip link set "${NSIF}p" netns "$NS"; sudo ip link set "$NSIF" up; sudo ip netns exec "$NS" ip link set "${NSIF}p" up; sudo ip netns exec "$NS" ip link set lo up; }
  netdown(){ sudo ip netns del "$NS" >/dev/null 2>&1; sudo ip link del "$NSIF" >/dev/null 2>&1; }
  netup; GT="$OUT/$tag.go.out"; :>"$GT"
  local gp gl; gp=$(gobpf program load file "$obj" --programs "$spec" -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  if [ "$type" = xdp ]; then capq "$GT" gobpf link attach xdp "$gp" "${NSIF}p" --netns "$NSPATH" --priority 100 -o json
  else capq "$GT" gobpf link attach "$type" "$gp" "${NSIF}p" ingress --netns "$NSPATH" --priority 100 -o json; fi
  gl=$(goj "$CAP_OUT" '.record.id'); capq "$GT" gobpf link get "$gl" -o json
  [ -n "$gl" ] && [ "$gl" != null ] && capq "$GT" gobpf link detach "$gl"; capq "$GT" gobpf program unload "$gp"; netdown
  netup; RT="$OUT/$tag.rust.out"; :>"$RT"
  local rp rl; rp=$(rsbpf load file --path "$obj" --programs "$spec" 2>/dev/null|awk '/Program ID:/{print $NF}')
  if [ "$type" = xdp ]; then capq "$RT" rsbpf attach "$rp" xdp --iface "${NSIF}p" --netns "$NSPATH" --priority 100
  else capq "$RT" rsbpf attach "$rp" "$type" --direction ingress --iface "${NSIF}p" --netns "$NSPATH" --priority 100; fi
  rl=$(rsf "$CAP_OUT" 'Link ID'); [ -n "$rl" ] && capq "$RT" rsbpf get link "$rl" && capq "$RT" rsbpf detach "$rl"; capq "$RT" rsbpf unload "$rp"; netdown
}

# Detach after the namespace path is deleted: attach inside a netns via
# --netns, destroy the netns (and its interface) with `ip netns del`,
# then detach + unload and observe whether bpfman state cleans up.
scen_ns_deleted_detach() { # TYPE OBJ SPEC
  local NS=bpfmanns NSPATH=/var/run/netns/bpfmanns NSIF=nsveth0 type=$1 obj=$2 spec=$3 tag="ns-deleted-detach-$1"
  netup(){ sudo ip netns del "$NS" >/dev/null 2>&1; sudo ip link del "$NSIF" >/dev/null 2>&1; sudo ip netns add "$NS"; sudo ip link add "$NSIF" type veth peer name "${NSIF}p"; sudo ip link set "${NSIF}p" netns "$NS"; sudo ip link set "$NSIF" up; sudo ip netns exec "$NS" ip link set "${NSIF}p" up; sudo ip netns exec "$NS" ip link set lo up; }
  delns(){ local T="$1"; echo "\$ sudo ip netns del $NS   # delete the namespace path" >>"$T"; sudo ip netns del "$NS"; echo "[exit $?]" >>"$T"; echo >>"$T"; }
  local gp gl rp rl
  netup; GT="$OUT/$tag.go.out"; :>"$GT"
  gp=$(gobpf program load file "$obj" --programs "$spec" -o json 2>/dev/null|jq -r '.programs[0].record.program_id')
  if [ "$type" = xdp ]; then capq "$GT" gobpf link attach xdp "$gp" "${NSIF}p" --netns "$NSPATH" --priority 100 -o json
  else capq "$GT" gobpf link attach "$type" "$gp" "${NSIF}p" ingress --netns "$NSPATH" --priority 100 -o json; fi
  gl=$(goj "$CAP_OUT" '.record.id'); capq "$GT" gobpf link get "$gl" -o json
  delns "$GT"
  capq "$GT" gobpf link get "$gl" -o json; capq "$GT" gobpf link detach "$gl"
  capq "$GT" gobpf link get "$gl" -o json; capq "$GT" gobpf program unload "$gp"; capq "$GT" gobpf program get "$gp" -o json
  sudo ip link del "$NSIF" >/dev/null 2>&1
  netup; RT="$OUT/$tag.rust.out"; :>"$RT"
  rp=$(rsbpf load file --path "$obj" --programs "$spec" 2>/dev/null|awk '/Program ID:/{print $NF}')
  if [ "$type" = xdp ]; then capq "$RT" rsbpf attach "$rp" xdp --iface "${NSIF}p" --netns "$NSPATH" --priority 100
  else capq "$RT" rsbpf attach "$rp" "$type" --direction ingress --iface "${NSIF}p" --netns "$NSPATH" --priority 100; fi
  rl=$(rsf "$CAP_OUT" 'Link ID'); capq "$RT" rsbpf get link "$rl"
  delns "$RT"
  capq "$RT" rsbpf get link "$rl"; capq "$RT" rsbpf detach "$rl"
  capq "$RT" rsbpf get link "$rl"; capq "$RT" rsbpf unload "$rp"; capq "$RT" rsbpf get program "$rp"
  sudo ip link del "$NSIF" >/dev/null 2>&1
}
