#!/usr/bin/env bash
#
# Run every REPL script under e2e/scripts/ against the built
# bpfman-shell binary. Each script executes from e2e/ so testdata
# paths match the Go e2e tests. The loop reports failures as it goes
# and exits non-zero at the end if any script failed.
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

fail=0
failed=""
for f in e2e/scripts/*.bpfman; do
    name=$(basename "$f")
    if [ -n "$filter" ] && ! echo "$name" | grep -q "$filter"; then
        continue
    fi
    printf "=== %s ===\n" "$name"
    if (cd e2e && sudo "../$bin_dir/bpfman-shell" "scripts/$name"); then
        echo "    pass: $name"
    else
        echo "    FAIL: $name"
        fail=1
        failed="$failed $name"
    fi
done

if [ "$fail" -ne 0 ]; then
    echo ""
    echo "failed:$failed"
    exit 1
fi

echo ""
echo "all REPL e2e scripts passed"
