#!/usr/bin/env bash
#
# Run every REPL script under e2e/scripts/ and e2e/new/ against the
# built bpfman-shell binary. Each script executes from e2e/ so
# testdata paths match the Go e2e tests. Scripts under e2e/new/
# spawn worker subprocesses via naked `bpfman-shell` invocations, so
# the harness puts BIN_DIR on PATH (preserving it across sudo) for
# those to resolve. The loop reports failures as it goes and exits
# non-zero at the end if any script failed.
#
# Usage: test-e2e-scripts.sh [filter]
#   filter  substring match against the script basename (optional)
#
# Environment:
#   BIN_DIR   directory containing the built bpfman-shell binary
#             (default: bin)

set -euo pipefail

filter=${1:-}
bin_dir=${BIN_DIR:-bin}
abs_bin_dir=$(cd "$bin_dir" && pwd)

fail=0
failed=""
for sub in scripts new; do
    for f in "e2e/$sub"/*.bpfman; do
        [ -e "$f" ] || continue
        name=$(basename "$f")
        if [ -n "$filter" ] && ! echo "$name" | grep -q "$filter"; then
            continue
        fi
        rel="$sub/$name"
        printf "=== %s ===\n" "$rel"
        if (cd e2e && sudo env "PATH=$abs_bin_dir:$PATH" "$abs_bin_dir/bpfman-shell" "$rel"); then
            echo "    pass: $rel"
        else
            echo "    FAIL: $rel"
            fail=1
            failed="$failed $rel"
        fi
    done
done

if [ "$fail" -ne 0 ]; then
    echo ""
    echo "failed:$failed"
    exit 1
fi

echo ""
echo "all REPL e2e scripts passed"
