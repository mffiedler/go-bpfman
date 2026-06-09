package ebpf

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestSummariseHelperStderr(t *testing.T) {
	t.Parallel()

	got := summariseHelperStderr("\nfirst line\n final reason \n\n")
	if got != "final reason" {
		t.Fatalf("summary mismatch: got %q", got)
	}
}

func TestHelperExitErrorIncludesHelperReason(t *testing.T) {
	t.Parallel()

	err := helperExitError("malloc", "/bin/bash", 1234, 1, "noise\nbpfman-ns: error: specific reason\n")
	if err == nil {
		t.Fatal("expected error")
	}
	got := err.Error()
	if !strings.Contains(got, "bpfman-ns failed attaching malloc to \"/bin/bash\" in container 1234 (exit 1): specific reason") {
		t.Fatalf("error did not include helper reason: %q", got)
	}
}

func TestSummariseHelperStderrKeepsNsexecLine(t *testing.T) {
	t.Parallel()

	line := "nsexec[123]: ERROR: setns failed"
	got := summariseHelperStderr(line + "\n")
	if got != line {
		t.Fatalf("summary mismatch: got %q", got)
	}
}

func TestHelperExitErrorWithoutHelperReason(t *testing.T) {
	t.Parallel()

	err := helperExitError("malloc", "/bin/bash", 1234, 1, "\n")
	if err == nil {
		t.Fatal("expected error")
	}
	got := err.Error()
	if strings.Contains(got, ": ") {
		t.Fatalf("error should not include empty helper reason: %q", got)
	}
}

func TestHelperReceiveErrorIncludesPreSendHelperReason(t *testing.T) {
	t.Parallel()

	waitErr := commandExitError(t, 7)
	recvErr := errors.New("recvmsg: connection reset by peer")
	err := helperReceiveError("malloc", "/bin/bash", 1234, recvErr, waitErr, "bpfman-ns: error: specific reason\n")
	if err == nil {
		t.Fatal("expected error")
	}
	got := err.Error()
	if !strings.Contains(got, "bpfman-ns failed attaching malloc to \"/bin/bash\" in container 1234 (exit 7): specific reason") {
		t.Fatalf("error did not include helper reason: %q", got)
	}
	if strings.Contains(got, "receive link fd from child") {
		t.Fatalf("helper reason should take precedence over receive error: %q", got)
	}
}

func TestHelperReceiveErrorFallsBackToReceiveError(t *testing.T) {
	t.Parallel()

	waitErr := commandExitError(t, 7)
	recvErr := errors.New("recvmsg: connection reset by peer")
	err := helperReceiveError("malloc", "/bin/bash", 1234, recvErr, waitErr, "\n")
	if err == nil {
		t.Fatal("expected error")
	}
	got := err.Error()
	if !strings.Contains(got, "receive link fd from child: recvmsg: connection reset by peer") {
		t.Fatalf("error did not include receive failure: %q", got)
	}
}

func commandExitError(t *testing.T, code int) error {
	t.Helper()

	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatal("expected command to fail")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exec.ExitError, got %T", err)
	}
	if exitErr.ExitCode() != code {
		t.Fatalf("exit code mismatch: got %d, want %d", exitErr.ExitCode(), code)
	}
	return err
}
