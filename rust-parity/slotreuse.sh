#!/usr/bin/env bash
# Slot reuse after detach + slot exhaustion, for XDP/TC dispatchers.
# Sourced after harness.sh. capq defined here if absent.
IF=bpfmanpar9
# Quiet capture that also exposes the exit code in global CAP_RC.
capq() { local T="$1"; shift; echo "\$ $*" >>"$T"; CAP_OUT="$("$@" 2>&1)"; CAP_RC=$?; { printf '%s\n' "$CAP_OUT"; echo "[exit $CAP_RC]"; echo; } >>"$T"; }

go_attach_t() { if [ "$1" = xdp ]; then capq "$GT" gobpf link attach xdp "$2" "$IF" --priority "$3" -o json; else capq "$GT" gobpf link attach tc "$2" "$IF" ingress --priority "$3" -o json; fi; }
rs_attach_t() { if [ "$1" = xdp ]; then capq "$RT" rsbpf attach "$2" xdp --iface "$IF" --priority "$3"; else capq "$RT" rsbpf attach "$2" tc --direction ingress --iface "$IF" --priority "$3"; fi; }
go_count() { gobpf dispatcher get "$1" "$2" "$3" -o json 2>/dev/null | jq '.members | length'; }
rs_count() { rsbpf list links 2>/dev/null | awk 'NR>1 && $1 ~ /^[0-9]+$/' | wc -l | tr -d ' '; }

slot_reuse() { # DTYPE TYPE OBJ SPEC
  local dtype=$1 type=$2 obj=$3 spec=$4
  local prios=(100 200 300 400 500 600 700 800 900 1000)
  echo "===== slot-reuse-$type (dispatcher $dtype) ====="

  # ---- GO ----
  GT="$OUT/slot-reuse-$type.go.out"; :>"$GT"; veth_up "$IF"
  capq "$GT" gobpf program load file "$obj" --programs "$spec" -o json
  local gp; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  declare -A gl
  for p in "${prios[@]}"; do go_attach_t "$type" "$gp" "$p"; gl[$p]=$(goj "$CAP_OUT" '.record.id'); done
  local nsid ifx; read nsid ifx < <(gobpf dispatcher list -o json 2>/dev/null | jq -r ".dispatchers[]|select(.key.type==\"$dtype\")|\"\(.key.nsid) \(.key.ifindex)\"")
  echo "  GO   full=$(go_count "$dtype" "$nsid" "$ifx")"
  capq "$GT" gobpf link detach "${gl[400]}"
  echo "  GO   after-detach-400=$(go_count "$dtype" "$nsid" "$ifx")"
  go_attach_t "$type" "$gp" 350; local gl350; gl350=$(goj "$CAP_OUT" '.record.id')
  capq "$GT" gobpf link get "$gl350" -o json; local gpos350; gpos350=$(goj "$CAP_OUT" '.record.details.position')
  echo "  GO   refilled=$(go_count "$dtype" "$nsid" "$ifx")  new-link(prio350).position=$gpos350 (expect 3)"
  for l in $(gobpf link list -o json 2>/dev/null | jq -r '.links[].id'); do capq "$GT" gobpf link detach "$l"; done
  capq "$GT" gobpf program unload "$gp"; veth_down "$IF"; unset gl

  # ---- RUST (no dispatcher cmd; use list-links count + link position) ----
  RT="$OUT/slot-reuse-$type.rust.out"; :>"$RT"; veth_up "$IF"
  capq "$RT" rsbpf load file --path "$obj" --programs "$spec"; local rp; rp=$(rsf "$CAP_OUT" 'Program ID')
  declare -A rl
  for p in "${prios[@]}"; do rs_attach_t "$type" "$rp" "$p"; rl[$p]=$(rsf "$CAP_OUT" 'Link ID'); done
  echo "  RUST full=$(rs_count)"
  capq "$RT" rsbpf detach "${rl[400]}"
  echo "  RUST after-detach-400=$(rs_count)"
  rs_attach_t "$type" "$rp" 350; local rl350; rl350=$(rsf "$CAP_OUT" 'Link ID')
  capq "$RT" rsbpf get link "$rl350"; local rpos350; rpos350=$(rsf "$CAP_OUT" 'Position')
  echo "  RUST refilled=$(rs_count)  new-link(prio350).position=$rpos350 (expect 3)"
  for l in $(rsbpf list links 2>/dev/null | awk 'NR>1 && $2 ~ /^[0-9]+$/ {print $2}'); do capq "$RT" rsbpf detach "$l"; done
  capq "$RT" rsbpf unload "$rp"; veth_down "$IF"; unset rl
}

slot_exhaust() { # DTYPE TYPE OBJ SPEC  -- fill 10 then 11th (prio 150) must fail
  local dtype=$1 type=$2 obj=$3 spec=$4
  local prios=(100 200 300 400 500 600 700 800 900 1000)
  echo "===== slot-exhaust-$type (11th attach must fail) ====="
  GT="$OUT/slot-exhaust-$type.go.out"; :>"$GT"; veth_up "$IF"
  capq "$GT" gobpf program load file "$obj" --programs "$spec" -o json; local gp; gp=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  for p in "${prios[@]}"; do go_attach_t "$type" "$gp" "$p"; done
  go_attach_t "$type" "$gp" 150
  echo "  GO   11th-attach exit=$CAP_RC msg: $(printf '%s' "$CAP_OUT" | grep -iE 'error|slot' | head -1 | sed 's/bpfman: //')"
  CAP_RC=0
  for l in $(gobpf link list -o json 2>/dev/null | jq -r '.links[].id'); do capq "$GT" gobpf link detach "$l"; done
  capq "$GT" gobpf program unload "$gp"; veth_down "$IF"
  RT="$OUT/slot-exhaust-$type.rust.out"; :>"$RT"; veth_up "$IF"
  capq "$RT" rsbpf load file --path "$obj" --programs "$spec"; local rp; rp=$(rsf "$CAP_OUT" 'Program ID')
  for p in "${prios[@]}"; do rs_attach_t "$type" "$rp" "$p"; done
  rs_attach_t "$type" "$rp" 150
  echo "  RUST 11th-attach exit=$CAP_RC msg: $(printf '%s' "$CAP_OUT" | grep -iE 'error|slot' | head -1)"
  for l in $(rsbpf list links 2>/dev/null | awk 'NR>1 && $2 ~ /^[0-9]+$/ {print $2}'); do capq "$RT" rsbpf detach "$l"; done
  capq "$RT" rsbpf unload "$rp"; veth_down "$IF"
}
