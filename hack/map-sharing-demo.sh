#!/usr/bin/env bash
#
# map-sharing-demo.sh -- show how this implementation and upstream Rust
# differ when BPF programs share maps.
#
# ELI5
# ----
# A BPF "map" is a shared table that a program reads and writes. Some
# programs keep a private table; sometimes two programs want to share
# one (e.g. a counter both update).
#
# When you load several programs and one lends its maps to the others,
# who is allowed to leave first?
#
#   Rust: the lender (owner) can leave whenever it likes. The shared
#         table is reference-counted and stays alive until the LAST
#         borrower is gone. Load order and unload order are free.
#
#   Go:   the borrowers are pinned underneath the owner's directory, so
#         the owner cannot leave while anyone is still borrowing. Trying
#         to unload the owner first is refused: "unload dependents
#         first". You must unload in reverse order.
#
# There is a second, sharper edge that this script does not exercise
# directly (it needs the gRPC server, which the operator uses): when the
# server loads several programs in ONE request, Go AUTOMATICALLY makes
# program 1 the owner and 2..n borrowers -- even when every program has
# its own private map and nothing is actually shared. Rust never does
# that; each program in a batch keeps its own maps. The CLI path shown
# below does NOT auto-share, so section 3 demonstrates the CLI matching
# Rust, and the auto-share is described rather than run.
#
# Usage:
#   sudo hack/map-sharing-demo.sh
#
# Override the binaries if they live elsewhere:
#   GO_BPFMAN=./bin/bpfman \
#   RUST_BPFMAN=~/src/github.com/bpfman/bpfman/target/debug/bpfman \
#   sudo -E hack/map-sharing-demo.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GO_BPFMAN=${GO_BPFMAN:-$REPO_ROOT/bin/bpfman}
RUST_BPFMAN=${RUST_BPFMAN:-$HOME/src/github.com/bpfman/bpfman/target/debug/bpfman}
OBJ=${OBJ:-$REPO_ROOT/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o}

if [[ $EUID -ne 0 ]]; then
	echo "This script needs root (it loads BPF programs). Re-run with sudo." >&2
	exit 1
fi

hr() { printf '\n=== %s ===\n' "$1"; }

# Extract a program id from the Go CLI's JSON load output.
go_id() {
	python3 -c 'import sys,json; print(json.load(sys.stdin)["programs"][0]["record"]["program_id"])'
}

# Extract a program id from the Rust CLI's single-program load output.
rust_id() {
	grep -oE 'Program ID:[[:space:]]+[0-9]+' | grep -oE '[0-9]+' | head -1
}

################################################################
# Section 1: Go refuses to unload the map OWNER first.
################################################################
GO_BASE=$(mktemp -d)
GO_RT=$GO_BASE/rt

go_cleanup() {
	# Best-effort: unload anything still loaded, unmount the bpffs the
	# runtime-dir created, then remove the scratch dir.
	[[ -n "${GO_IDB:-}" ]] && "$GO_BPFMAN" --runtime-dir "$GO_RT" program unload "$GO_IDB" >/dev/null 2>&1 || true
	[[ -n "${GO_IDA:-}" ]] && "$GO_BPFMAN" --runtime-dir "$GO_RT" program unload "$GO_IDA" >/dev/null 2>&1 || true
	umount "$GO_RT/fs" >/dev/null 2>&1 || true
	rm -rf "$GO_BASE" >/dev/null 2>&1 || true
}
trap go_cleanup EXIT

hr "GO: load program A (becomes the map owner)"
GO_IDA=$("$GO_BPFMAN" --runtime-dir "$GO_RT" program load file "$OBJ" --programs kprobe:mkp_a -o json | go_id)
echo "owner A = $GO_IDA"

hr "GO: load program B sharing A's maps (--map-owner-id $GO_IDA)"
GO_IDB=$("$GO_BPFMAN" --runtime-dir "$GO_RT" program load file "$OBJ" --programs kprobe:mkp_b --map-owner-id "$GO_IDA" -o json | go_id)
echo "borrower B = $GO_IDB"
echo "B's maps are pinned under A's directory:"
"$GO_BPFMAN" --runtime-dir "$GO_RT" program get "$GO_IDB" -o json \
	| python3 -c 'import sys,json;print("  map_pin_path:",json.load(sys.stdin)["record"]["handles"]["map_pin_path"])'

hr "GO: try to unload the OWNER A first -- EXPECT REFUSAL"
if "$GO_BPFMAN" --runtime-dir "$GO_RT" program unload "$GO_IDA"; then
	echo ">> Go ALLOWED it (unexpected)."
else
	echo ">> Go REFUSED it (this is the divergence)."
fi

hr "GO: the supported order is borrower B first, then owner A"
"$GO_BPFMAN" --runtime-dir "$GO_RT" program unload "$GO_IDB" && echo "  unloaded B"
"$GO_BPFMAN" --runtime-dir "$GO_RT" program unload "$GO_IDA" && echo "  unloaded A"
GO_IDA= ; GO_IDB=

################################################################
# Section 2: Rust lets the OWNER leave; the shared maps survive.
################################################################
if [[ ! -x "$RUST_BPFMAN" ]]; then
	hr "RUST: binary not found at $RUST_BPFMAN -- skipping the Rust half"
	echo "Set RUST_BPFMAN=/path/to/rust/bpfman to run it."
	exit 0
fi

rust_cleanup() {
	for id in ${RUST_IDB:-} ${RUST_IDA:-}; do
		"$RUST_BPFMAN" unload "$id" >/dev/null 2>&1 || true
	done
}
trap 'go_cleanup; rust_cleanup' EXIT

hr "RUST: load program A (becomes the map owner)"
RUST_IDA=$("$RUST_BPFMAN" load file --programs kprobe:mkp_a --path "$OBJ" | rust_id)
echo "owner A = $RUST_IDA"

hr "RUST: load program B sharing A's maps (--map-owner-id $RUST_IDA)"
RUST_IDB=$("$RUST_BPFMAN" load file --programs kprobe:mkp_b --path "$OBJ" --map-owner-id "$RUST_IDA" | rust_id)
echo "borrower B = $RUST_IDB"

hr "RUST: unload the OWNER A first -- EXPECT SUCCESS"
if "$RUST_BPFMAN" unload "$RUST_IDA"; then
	echo ">> Rust ALLOWED it (refcounted)."
	RUST_IDA=
else
	echo ">> Rust REFUSED it (unexpected)."
fi

hr "RUST: borrower B still has its (shared) maps after the owner left"
"$RUST_BPFMAN" get program "$RUST_IDB" 2>/dev/null | grep -iE "Map Pin Path|Map Owner ID|Maps Used By" | sed 's/^/  /'

hr "RUST: unload borrower B"
"$RUST_BPFMAN" unload "$RUST_IDB" >/dev/null 2>&1 && echo "  unloaded B"
RUST_IDB=

################################################################
# Section 3: the Go CLI does NOT auto-share a multi-program load.
################################################################
GO_BASE2=$(mktemp -d)
GO_RT2=$GO_BASE2/rt
go2_cleanup() {
	"$GO_BPFMAN" --runtime-dir "$GO_RT2" program list -o json 2>/dev/null \
		| python3 -c 'import sys,json
try:
    for p in json.load(sys.stdin): print(p["record"]["program_id"])
except Exception: pass' 2>/dev/null \
		| while read -r id; do "$GO_BPFMAN" --runtime-dir "$GO_RT2" program unload "$id" >/dev/null 2>&1 || true; done
	umount "$GO_RT2/fs" >/dev/null 2>&1 || true
	rm -rf "$GO_BASE2" >/dev/null 2>&1 || true
}
trap 'go_cleanup; rust_cleanup; go2_cleanup' EXIT

hr "GO CLI: load A and B in ONE command -- no implicit owner is set"
"$GO_BPFMAN" --runtime-dir "$GO_RT2" program load file "$OBJ" --programs kprobe:mkp_a,kprobe:mkp_b -o json \
	| python3 -c 'import sys,json
for p in json.load(sys.stdin)["programs"]:
    r=p["record"]; print("  prog %-7s map_owner_id=%s" % (r["load"]["program_name"], r["load"]["map_owner_id"]))'
echo
echo "Both show map_owner_id=None: the CLI matches Rust (each keeps its own maps)."
echo "The gRPC SERVER path is the one that forces program 1 to own the rest;"
echo "that auto-ownership is what makes 'unload in load order' fail under the"
echo "operator, reproducing section 1 without anyone asking to share."
