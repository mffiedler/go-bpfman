"""Worker for the e2e/new/ DSL translation experiment.

Blocks on a "go" sentinel file; once it exists, issues N unlink
syscalls and exits. Stable PID throughout (single python process)
so the BPF program's expected_pid filter matches every event.

Usage: python3 _worker.py SENTINEL N [OUT]
"""

import os
import sys
import time


def main() -> int:
    sentinel = sys.argv[1]
    n = int(sys.argv[2])
    out = sys.argv[3] if len(sys.argv) > 3 else None

    while not os.path.exists(sentinel):
        time.sleep(0.01)

    log = open(out, "w") if out else None
    if log:
        log.write(f"pid={os.getpid()}\n")
    for i in range(n):
        path = f"/tmp/bpfman-shell-wf.{os.getpid()}.{i}"
        open(path, "w").close()
        os.unlink(path)
        if log:
            log.write(f"i={i}\n")
    if log:
        log.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
