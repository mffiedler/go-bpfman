#!/usr/bin/env bash
#
# Drive all four e2e cleanup scripts in the order they need to run.
# A single entry point for "give me a quiescent system again": loop
# this before reproducing a flake, or chain it into a stress harness
# between iterations.
#
# Order matters and is fixed:
#
#   1. cleanup-all-dispatchers.sh -- detach every bpfman dispatcher
#      so subsequent steps see no live attachments.
#   2. cleanup-test-dispatchers.sh -- drain any test-only dispatchers
#      left over after step 1 (separate set; do not collapse).
#   3. cleanup-test-veths.sh -- remove host-side test veth peers
#      before the netns they lived in disappears.
#   4. cleanup-test-netns.sh -- delete the now-orphaned netns.
#
# Each step is idempotent and safe on a quiescent system. The wrapper
# does not stop on the first failure; each child reports its own
# status and the exit code is the OR of all four.
#
# Usage: sudo hack/cleanup-test-resources.sh

set -uo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "error: must be run as root (sudo $0)" >&2
    exit 1
fi

here=$(cd "$(dirname "$0")" && pwd)
rc=0

for step in \
    cleanup-all-dispatchers.sh \
    cleanup-test-dispatchers.sh \
    cleanup-test-veths.sh \
    cleanup-test-netns.sh
do
    if ! "$here/$step"; then
        rc=1
    fi
done

exit "$rc"
