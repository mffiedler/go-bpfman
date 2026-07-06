package main

import "testing"

// TestREADMEUpToDate fails if the generated Rust parity section in the
// repository README is stale relative to rust-parity/cases.yaml and the
// transcripts. Regenerate with `make parity-readme`. Paths are relative
// to this package directory (rust-parity/cmd/parity-readme).
func TestREADMEUpToDate(t *testing.T) {
	t.Parallel()
	const (
		cases  = "../../cases.yaml"
		readme = "../../../README.md"
	)
	if err := run(cases, readme, true); err != nil {
		t.Fatalf("%v", err)
	}
}
