#!/usr/bin/env bash
#
# map-sharing-pinned-demo.sh -- companion to map-sharing-demo.sh.
#
# Question this answers: if we stop the gRPC server fabricating map
# ownership for multi-program loads, do programs that GENUINELY share a
# LIBBPF_PIN_BY_NAME map still share it?
#
# The object loaded here has two kprobe programs (mkp_x, mkp_y) that
# both reference one LIBBPF_PIN_BY_NAME map (shared_kprobe_map). It is
# loaded through the Go CLI, which -- unlike the gRPC server -- does NOT
# force-share. So this isolates the by-name sharing path:
#
#   * if both programs report map_owner_id=None AND the same kernel map
#     id for shared_kprobe_map, then by-name sharing works on its own and
#     dropping the forced-share is safe for real shared maps.
#
# Usage:
#   sudo hack/map-sharing-pinned-demo.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GO_BPFMAN=${GO_BPFMAN:-$REPO_ROOT/bin/bpfman}
OBJ=${OBJ:-$REPO_ROOT/e2e/testdata/bpf/multi_prog_kprobe_shared_pinned.bpf.o}

if [[ $EUID -ne 0 ]]; then
	echo "This script needs root (it loads BPF programs). Re-run with sudo." >&2
	exit 1
fi

if [[ ! -f "$OBJ" ]]; then
	echo "Missing $OBJ -- build it with:" >&2
	echo "  make e2e/testdata/bpf/multi_prog_kprobe_shared_pinned.bpf.o" >&2
	exit 1
fi

BASE=$(mktemp -d); RT=$BASE/rt
cleanup() {
	"$GO_BPFMAN" --runtime-dir "$RT" program list -o json 2>/dev/null \
		| python3 -c 'import sys,json
try:
    for p in json.load(sys.stdin): print(p["record"]["program_id"])
except Exception: pass' 2>/dev/null \
		| while read -r id; do "$GO_BPFMAN" --runtime-dir "$RT" program unload "$id" >/dev/null 2>&1 || true; done
	umount "$RT/fs" >/dev/null 2>&1 || true
	rm -rf "$BASE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

printf '\n=== GO CLI: one multi-program load of two programs sharing a PIN_BY_NAME map ===\n'
OUT=$("$GO_BPFMAN" --runtime-dir "$RT" program load file "$OBJ" --programs kprobe:mkp_x,kprobe:mkp_y -o json)

echo "$OUT" | python3 -c '
import sys, json
progs = json.load(sys.stdin)["programs"]
ids = {}
for p in progs:
    r = p["record"]
    name = r["load"]["program_name"]
    owner = r["load"]["map_owner_id"]
    # The kernel truncates map names to 15 chars, so match by prefix.
    mid = next((m["id"] for m in p["status"]["maps"] if "shared_kprobe_map".startswith(m["name"])), None)
    ids[name] = mid
    print("  prog %-6s map_owner_id=%-5s shared_kprobe_map kernel id=%s" % (name, owner, mid))
xs = list(ids.values())
print()
if len(xs) == 2 and xs[0] is not None and xs[0] == xs[1]:
    print(">> SHARED: both programs resolve to the SAME kernel map id (%s)." % xs[0])
    print(">> By-name sharing works with no map-owner relationship.")
    print(">> Dropping the gRPC forced-share is SAFE for genuine PIN_BY_NAME maps.")
else:
    print(">> NOT shared: programs got different/absent map ids %s." % xs)
    print(">> Dropping forced-share WOULD regress this case -- investigate before proceeding.")
'

printf '\n=== the by-name shared pin on bpffs ===\n'
find "$RT/fs/shared/" -mindepth 1 -maxdepth 1 -printf '%M %u %g %s %f\n' 2>&1 | sed 's/^/  /'
