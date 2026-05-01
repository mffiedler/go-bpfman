//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// workloadCommand is a single instruction sent from a parent test
// process to a workload driver subprocess over its stdin. The driver
// fires the requested action in its own PID context so a BPF program
// can filter on bpf_get_current_pid_tgid() == driver-pid.
//
// Op values:
//   - "kill"   send SIGUSR1 to self N times (fires sys_enter_kill).
//   - "unlink" create then os.Remove() N temp files (fires
//     sys_enter_unlinkat / do_unlinkat).
//   - "uprobe" call e2e_uprobe_call_malloc N times (fires whichever
//     uprobe / uretprobe is attached to the symbol).
//   - "exit"   exit the driver with status 0.
type workloadCommand struct {
	Op string `json:"op"`
	N  int    `json:"n,omitempty"`
}

// workloadAck is the per-command acknowledgement written back to the
// parent over stdout. Err is non-empty if the command failed.
type workloadAck struct {
	Op  string `json:"op"`
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// runWorkloadDriver implements the BPFMAN_E2E_MODE=workload-driver
// helper role. It reads NDJSON commands from stdin, executes each in
// its own PID context, and writes one NDJSON ack per command to
// stdout. Exits 0 on stdin EOF or on receiving {"op":"exit"}.
func runWorkloadDriver() {
	tmpDir, err := os.MkdirTemp("", "bpfman-e2e-workload-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "workload-driver: mktemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 64*1024)
	out := json.NewEncoder(os.Stdout)

	seq := 0
	for in.Scan() {
		var cmd workloadCommand
		if err := json.Unmarshal(in.Bytes(), &cmd); err != nil {
			_ = out.Encode(workloadAck{Op: "decode", Err: err.Error()})
			continue
		}
		ack := workloadAck{Op: cmd.Op, OK: true}
		switch cmd.Op {
		case "kill":
			pid := os.Getpid()
			for i := 0; i < cmd.N; i++ {
				if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
					ack.OK, ack.Err = false, err.Error()
					break
				}
			}
		case "unlink":
			for i := 0; i < cmd.N; i++ {
				path := filepath.Join(tmpDir, fmt.Sprintf("workload-%d", seq))
				seq++
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					ack.OK, ack.Err = false, err.Error()
					break
				}
				if err := os.Remove(path); err != nil {
					ack.OK, ack.Err = false, err.Error()
					break
				}
			}
		case "uprobe":
			for i := 0; i < cmd.N; i++ {
				invokeUprobeCallMalloc()
			}
		case "exit":
			_ = out.Encode(ack)
			return
		default:
			ack.OK, ack.Err = false, "unknown op: "+cmd.Op
		}
		if err := out.Encode(ack); err != nil {
			fmt.Fprintf(os.Stderr, "workload-driver: encode: %v\n", err)
			os.Exit(1)
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "workload-driver: read stdin: %v\n", err)
		os.Exit(1)
	}
}
