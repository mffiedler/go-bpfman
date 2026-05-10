"""Multi-wave kill-syscall worker for the tracepoint multi-prog test.

Each os.kill(self_pid, SIGUSR1) issues a kill(2) syscall in this
process's PID context, which fires the syscalls/sys_enter_kill
tracepoint exactly once. SIGUSR1's default disposition would
terminate us, so the handler is replaced with a no-op before any
kills go out.

Stable PID across K waves; each wave gates on a numbered "go"
sentinel and writes a numbered ack file the script can poll.

Usage:
    python3 _worker_multi_kill.py SENTINEL_PREFIX ACK_PREFIX N K
"""

import os
import signal
import sys
import time


def main() -> int:
    sentinel_prefix = sys.argv[1]
    ack_prefix = sys.argv[2]
    n = int(sys.argv[3])
    k = int(sys.argv[4])

    signal.signal(signal.SIGUSR1, lambda s, f: None)
    pid = os.getpid()

    for wave in range(1, k + 1):
        sentinel = f"{sentinel_prefix}.{wave}"
        ack = f"{ack_prefix}.{wave}"
        while not os.path.exists(sentinel):
            time.sleep(0.01)
        for _ in range(n):
            os.kill(pid, signal.SIGUSR1)
        open(ack, "w").close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
