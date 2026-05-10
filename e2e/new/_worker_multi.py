"""Multi-wave worker for the e2e/new/ DSL translation experiment.

Stable PID across K waves of N unlink syscalls each. Each wave is
gated by a numbered "go" sentinel and acked via a numbered ack
file. The script removes sentinels and acks (under bpfman-shell's
own PID, not the worker's, so those operations do not register
against the BPF program's expected_pid filter).

Usage:
    python3 _worker_multi.py SENTINEL_PREFIX ACK_PREFIX N K

For wave w (1-indexed), block until SENTINEL_PREFIX.w exists, then
issue N unlink syscalls, then create ACK_PREFIX.w. Exit 0 after
wave K's ack.
"""

import os
import sys
import time


def main() -> int:
    sentinel_prefix = sys.argv[1]
    ack_prefix = sys.argv[2]
    n = int(sys.argv[3])
    k = int(sys.argv[4])

    for wave in range(1, k + 1):
        sentinel = f"{sentinel_prefix}.{wave}"
        ack = f"{ack_prefix}.{wave}"
        while not os.path.exists(sentinel):
            time.sleep(0.01)
        for i in range(n):
            path = f"/tmp/bpfman-shell-wf.{os.getpid()}.{wave}.{i}"
            open(path, "w").close()
            os.unlink(path)
        open(ack, "w").close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
