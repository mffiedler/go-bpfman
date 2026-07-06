package main

import (
	"io"
	"testing"
)

// TestKernelVerdictsMatchExpectations fails if any case's kernel-judged
// verdict (computed from the committed bpftool footprints) contradicts
// the expectation declared in cases.yaml -- for example if Rust starts
// honouring kretprobe, or a captured footprint diverges unexpectedly.
// Re-capture with rust-parity/kernel-capture.sh; verdicts are declared
// in cases.yaml under kernel_cases.
func TestKernelVerdictsMatchExpectations(t *testing.T) {
	t.Parallel()
	const (
		cases  = "../../cases.yaml"
		outdir = "../../outputs"
	)
	failures, err := verify(cases, outdir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		// Re-run with output to show which cases contradict.
		_, _ = verify(cases, outdir, testWriter{t})
		t.Fatalf("%d kernel case(s) contradict their declared expectation", failures)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
