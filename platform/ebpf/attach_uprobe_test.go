package ebpf

import (
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
