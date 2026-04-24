#!/usr/bin/env bash
#
# Run every REPL script under examples/ against the built bpfman
# binary. Each script executes from the repo root because the
# scripts reference testdata with paths like
# "e2e/testdata/bpf/<obj>.bpf.o". The loop reports failures as it
# goes and exits non-zero at the end if any script failed.
#
# Usage: test-examples.sh [filter]
#   filter  substring match against the script basename (optional)
#
# Environment:
#   BIN_DIR   directory containing the built bpfman binary
#             (default: bin)

set -euo pipefail

filter=${1:-}
bin_dir=${BIN_DIR:-bin}

fail=0
failed=""
for f in examples/*.bpfman; do
    name=$(basename "$f")
    if [ -n "$filter" ] && ! echo "$name" | grep -q "$filter"; then
        continue
    fi
    printf "=== %s ===\n" "$name"
    if sudo "$bin_dir/bpfman" repl -f "$f"; then
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
echo "all example scripts passed"
