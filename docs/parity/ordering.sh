#!/usr/bin/env bash
# Comprehensive priority-ordering proofs. Sourced after harness.sh.
# capq (quiet capture) is defined here too in case ordering.sh isn't sourced.
IF=bpfmanpar9
capq() { local T="$1"; shift; echo "\$ $*" >>"$T"; CAP_OUT="$("$@" 2>&1)"; CAP_RC=$?; { printf '%s\n' "$CAP_OUT"; echo "[exit $CAP_RC]"; echo; } >>"$T"; }

# --- XDP/TC dispatcher chain: one program attached at scrambled priorities ---
chain_disp() { # TAG DTYPE TYPE OBJ SPEC PRIOS...
  local tag=$1 dtype=$2 type=$3 obj=$4 spec=$5; shift 5
  local prios=("$@") gp rp l nsid ifx
  echo "===== $tag : priorities ${prios[*]} (dispatcher $dtype) ====="
  # GO
  GT="$OUT/$tag.go.out"; :>"$GT"; veth_up "$IF"
  capq "$GT" gobpf program load file "$obj" --programs "$spec" -o json
  gp=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  for p in "${prios[@]}"; do
    if [ "$type" = xdp ]; then capq "$GT" gobpf link attach xdp "$gp" "$IF" --priority "$p" -o json
    else capq "$GT" gobpf link attach tc "$gp" "$IF" ingress --priority "$p" -o json; fi
  done
  read nsid ifx < <(gobpf dispatcher list -o json 2>/dev/null | jq -r ".dispatchers[]|select(.key.type==\"$dtype\")|\"\(.key.nsid) \(.key.ifindex)\"")
  capq "$GT" gobpf dispatcher get "$dtype" "$nsid" "$ifx" -o json
  echo -n "  GO   per-link prio:pos -> "; for l in $(gobpf link list -o json 2>/dev/null | jq -r '.links[].id'); do capq "$GT" gobpf link get "$l" -o json; echo -n "$(goj "$CAP_OUT" '.record.details|"\(.priority):\(.position)"') "; done; echo
  echo -n "  GO   dispatcher order (pos=prio) -> "; capq "$GT" gobpf dispatcher get "$dtype" "$nsid" "$ifx" -o json; goj "$CAP_OUT" '.members[]|"\(.position)=\(.priority)"' | tr '\n' ' '; echo
  for l in $(gobpf link list -o json 2>/dev/null | jq -r '.links[].id'); do capq "$GT" gobpf link detach "$l"; done
  capq "$GT" gobpf program unload "$gp"; veth_down "$IF"
  # RUST (no dispatcher command; per-link positions only)
  RT="$OUT/$tag.rust.out"; :>"$RT"; veth_up "$IF"
  capq "$RT" rsbpf load file --path "$obj" --programs "$spec"; rp=$(rsf "$CAP_OUT" 'Program ID')
  local rlinks=()
  for p in "${prios[@]}"; do
    if [ "$type" = xdp ]; then capq "$RT" rsbpf attach "$rp" xdp --iface "$IF" --priority "$p"
    else capq "$RT" rsbpf attach "$rp" tc --direction ingress --iface "$IF" --priority "$p"; fi
    rlinks+=("$(rsf "$CAP_OUT" 'Link ID')")
  done
  echo -n "  RUST per-link prio:pos -> "; for l in "${rlinks[@]}"; do capq "$RT" rsbpf get link "$l"; echo -n "$(rsf "$CAP_OUT" 'Priority'):$(rsf "$CAP_OUT" 'Position') "; done; echo
  for l in "${rlinks[@]}"; do capq "$RT" rsbpf detach "$l"; done
  capq "$RT" rsbpf unload "$rp"; veth_down "$IF"
}

# --- TCX chain: DISTINCT programs (native mprog rejects same prog twice) ---
chain_tcx() { # TAG OBJ SPEC PRIOS...
  local tag=$1 obj=$2 spec=$3; shift 3
  local prios=("$@") gp rp l
  echo "===== $tag : priorities ${prios[*]} (tcx, distinct programs) ====="
  GT="$OUT/$tag.go.out"; :>"$GT"; veth_up "$IF"
  local gpids=() glinks=()
  for p in "${prios[@]}"; do
    capq "$GT" gobpf program load file "$obj" --programs "$spec" -o json; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id'); gpids+=("$gp")
    capq "$GT" gobpf link attach tcx "$gp" "$IF" ingress --priority "$p" -o json; glinks+=("$(goj "$CAP_OUT" '.record.id')")
  done
  echo -n "  GO   per-link prio:pos -> "; for l in "${glinks[@]}"; do capq "$GT" gobpf link get "$l" -o json; echo -n "$(goj "$CAP_OUT" '.record.details|"\(.priority):\(.position)"') "; done; echo
  for l in "${glinks[@]}"; do capq "$GT" gobpf link detach "$l"; done
  for gp in "${gpids[@]}"; do capq "$GT" gobpf program unload "$gp"; done; veth_down "$IF"
  RT="$OUT/$tag.rust.out"; :>"$RT"; veth_up "$IF"
  local rpids=() rlinks=()
  for p in "${prios[@]}"; do
    capq "$RT" rsbpf load file --path "$obj" --programs "$spec"; rp=$(rsf "$CAP_OUT" 'Program ID'); rpids+=("$rp")
    capq "$RT" rsbpf attach "$rp" tcx --direction ingress --iface "$IF" --priority "$p"; rlinks+=("$(rsf "$CAP_OUT" 'Link ID')")
  done
  echo -n "  RUST per-link prio:pos -> "; for l in "${rlinks[@]}"; do capq "$RT" rsbpf get link "$l"; echo -n "$(rsf "$CAP_OUT" 'Priority'):$(rsf "$CAP_OUT" 'Position') "; done; echo
  for l in "${rlinks[@]}"; do capq "$RT" rsbpf detach "$l"; done
  for rp in "${rpids[@]}"; do capq "$RT" rsbpf unload "$rp"; done; veth_down "$IF"
}
