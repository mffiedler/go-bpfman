//go:build e2e

package e2e

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// Workload is a handle to a workload-driver subprocess (re-execed
// e2e.test under BPFMAN_E2E_MODE=workload-driver). It exposes
// ergonomic methods for firing fixed bursts of syscalls in its own
// PID context, so BPF programs that filter on the driver's PID can
// assert exact counts even when many parallel tests share a binary.
type Workload struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	enc     *json.Encoder
	pid     int
	stopped bool
}

// Pid returns the workload-driver subprocess's kernel PID.
func (w *Workload) Pid() int { return w.pid }

// PidBytes returns Pid as little-endian uint32 bytes, the form
// required by bpfman GlobalData["expected_pid"] when the BPF program
// declares `volatile const __u32 expected_pid`.
func (w *Workload) PidBytes() []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(w.pid))
	return b[:]
}

// Kill sends N SIGUSR1 signals to the driver's own PID, waiting for
// the driver's NDJSON ack before returning. Each signal fires
// sys_enter_kill in the driver's PID context.
func (w *Workload) Kill(n int) {
	w.t.Helper()
	w.do(workloadCommand{Op: "kill", N: n})
}

// Unlink creates and os.Remove()s N temp files inside the driver's
// own temp dir, waiting for the ack before returning. Each remove
// fires sys_enter_unlinkat and do_unlinkat in the driver's PID
// context.
func (w *Workload) Unlink(n int) {
	w.t.Helper()
	w.do(workloadCommand{Op: "unlink", N: n})
}

// Uprobe calls e2e_uprobe_call_malloc N times in the driver's own
// PID, waiting for the ack before returning. Each call fires
// whichever uprobe (or uretprobe) is attached to the symbol.
func (w *Workload) Uprobe(n int) {
	w.t.Helper()
	w.do(workloadCommand{Op: "uprobe", N: n})
}

func (w *Workload) do(cmd workloadCommand) {
	w.t.Helper()
	require.NoError(w.t, w.enc.Encode(cmd), "send workload command %+v", cmd)
	line, err := w.stdout.ReadBytes('\n')
	require.NoError(w.t, err, "read workload ack for %+v", cmd)
	var ack workloadAck
	require.NoError(w.t, json.Unmarshal(line, &ack), "decode workload ack: %s", line)
	require.Equal(w.t, cmd.Op, ack.Op, "workload ack op mismatch")
	require.True(w.t, ack.OK, "workload command %+v failed: %s", cmd, ack.Err)
}

// Close tells the driver to exit and waits for the subprocess. Safe
// to call multiple times; subsequent calls are no-ops.
func (w *Workload) Close() {
	if w.stopped {
		return
	}
	w.stopped = true
	_ = w.enc.Encode(workloadCommand{Op: "exit"})
	_, _ = w.stdout.ReadBytes('\n') // best-effort; ignore error
	_ = w.stdin.Close()
	_ = w.cmd.Wait()
}

// startWorkload spawns the workload-driver subprocess and registers
// a t.Cleanup that closes it. The returned Workload is ready for
// Kill / Unlink calls.
func startWorkload(t *testing.T) *Workload {
	t.Helper()
	require.NotEmpty(t, selfExe, "selfExe not initialised; TestMain must have run")

	cmd := exec.Command(selfExe)
	cmd.Env = append(os.Environ(), e2eModeEnv+"="+e2eModeWorkloadDriver)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "workload stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "workload stdout pipe")
	require.NoError(t, cmd.Start(), "start workload driver")

	w := &Workload{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		enc:    json.NewEncoder(stdin),
		pid:    cmd.Process.Pid,
	}
	t.Cleanup(w.Close)
	return w
}
