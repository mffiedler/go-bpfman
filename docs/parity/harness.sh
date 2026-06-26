#!/usr/bin/env bash
# Parity proof harness. Deliberately NOT `set -e`: we want to record
# failures per step, not abort the whole run. Each program type calls
# cap() for every command, which tees the command + output + exit code
# into a per-impl transcript and to stdout.
set -uo pipefail

# Repo root. When this file is run/sourced via bash, derive it from the
# script location (docs/parity/harness.sh -> repo root is ../..).
# Override with PARITY_REPO; fall back to the conventional checkout path
# for interactive sourcing under shells without BASH_SOURCE.
REPO="${PARITY_REPO:-}"
if [ -z "$REPO" ] && [ -n "${BASH_SOURCE:-}" ]; then
  REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)"
fi
REPO="${REPO:-$HOME/src/github.com/frobware/go-bpfman}"
GO="$REPO/bin/go-bpfman"
RS="$REPO/bin/bpfman-rs"
GO_RT=/run/bpfman-parity-go
OUT="$REPO/docs/parity/outputs"
mkdir -p "$OUT"

# Go always runs with the isolated runtime dir.
gobpf() { sudo "$GO" --runtime-dir "$GO_RT" "$@"; }
rsbpf() { sudo "$RS" "$@"; }

# cap TRANSCRIPT CMD...  -> runs CMD, appends to TRANSCRIPT and stdout.
# Exposes the captured stdout+stderr in global CAP_OUT and rc in CAP_RC.
cap() {
  local T="$1"; shift
  { echo "\$ $*"; } | tee -a "$T"
  CAP_OUT="$("$@" 2>&1)"; CAP_RC=$?
  printf '%s\n' "$CAP_OUT" | tee -a "$T"
  { echo "[exit $CAP_RC]"; echo; } | tee -a "$T"
  return 0
}

# Interface helpers (recorded recipe).
veth_up() {
  local host="$1" peer="${1}p"
  sudo ip link del "$host" >/dev/null 2>&1 || true
  sudo ip link add "$host" type veth peer name "$peer"
  sudo ip link set "$host" up
  sudo ip link set "$peer" up
}
veth_down() { sudo ip link del "$1" >/dev/null 2>&1 || true; }

# JSON field extractor for Go output.
goj() { printf '%s' "$1" | jq -r "$2" 2>/dev/null; }
# Text field extractor for Rust output. Rust prints a right-padded
# "Bpfman State" block with a leading space before each label, e.g.
# " Program ID:   256408 ". Trim leading space, match "FIELD:", strip
# the label and surrounding whitespace.
rsf() {
  awk -v f="$2" '
    { line=$0; sub(/^[[:space:]]+/,"",line) }
    index(line, f":")==1 {
      sub(/^[^:]*:[[:space:]]*/,"",line);
      sub(/[[:space:]]+$/,"",line);
      print line; exit
    }' <<<"$1"
}

# ---- lifecycle helpers ----
# go_begin TAG OBJ SPEC : load + get program. Sets GT, GO_PID.
go_begin() {
  GT="$OUT/$1.go.out"; : > "$GT"
  cap "$GT" gobpf program load file "$2" --programs "$3" --metadata parity=go -o json
  GO_PID=$(goj "$CAP_OUT" '.programs[0].record.program_id')
  echo ">>> GO_PID=$GO_PID"
  cap "$GT" gobpf program get "$GO_PID" -o json
}
# go_finish : get link (GO_LINK set by caller) + list + detach + unload + get(fail)
go_finish() {
  echo ">>> GO_LINK=$GO_LINK"
  cap "$GT" gobpf link get "$GO_LINK" -o json
  cap "$GT" gobpf link list -o json
  cap "$GT" gobpf program list -o json
  cap "$GT" gobpf link detach "$GO_LINK"
  cap "$GT" gobpf program unload "$GO_PID"
  cap "$GT" gobpf program get "$GO_PID" -o json
}
# rust_begin TAG OBJ SPEC : load + get program. Sets RT, R_PID.
rust_begin() {
  RT="$OUT/$1.rust.out"; : > "$RT"
  cap "$RT" rsbpf load file --path "$2" --programs "$3" --metadata parity=rust
  R_PID=$(rsf "$CAP_OUT" 'Program ID')
  echo ">>> R_PID=$R_PID"
  cap "$RT" rsbpf get program "$R_PID"
}
# rust_finish : get link (R_LINK set by caller) + list + detach + unload + get(fail)
rust_finish() {
  echo ">>> R_LINK=$R_LINK"
  cap "$RT" rsbpf get link "$R_LINK"
  cap "$RT" rsbpf list programs
  cap "$RT" rsbpf list links
  cap "$RT" rsbpf detach "$R_LINK"
  cap "$RT" rsbpf unload "$R_PID"
  cap "$RT" rsbpf get program "$R_PID"
}
