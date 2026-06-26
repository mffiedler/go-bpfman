// Command parity-compare judges Go vs Rust parity from kernel state,
// not from either tool's self-report. For each case it reads the two
// bpftool-captured footprints under docs/parity/outputs/kernel/
// (program tag/type, map shape, link semantics), diffs them, and checks
// the computed verdict against the expectation declared in
// docs/parity/cases.yaml. bpftool is the neutral juror; this tool only
// diffs what bpftool saw.
//
// Usage:
//
//	parity-compare           print the kernel-judged verdict table
//	parity-compare -check    exit non-zero if any verdict contradicts
//	                         its declared expectation
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/frobware/go-bpfman/internal/parity"

	"gopkg.in/yaml.v3"
)

type cfg struct {
	KernelCases    []parity.KernelCase `yaml:"kernel_cases"`
	BehaviourCases []parity.KernelCase `yaml:"behaviour_cases"`
}

func main() {
	check := flag.Bool("check", false, "exit non-zero if a verdict contradicts its declared expectation")
	casesPath := flag.String("cases", "docs/parity/cases.yaml", "path to the cases manifest")
	outdir := flag.String("outdir", "docs/parity/outputs", "directory of captured observations (kernel/ and behaviour/)")
	flag.Parse()

	failures, err := verify(*casesPath, *outdir, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parity-compare:", err)
		os.Exit(1)
	}
	if *check && failures > 0 {
		fmt.Fprintf(os.Stderr, "parity-compare: %d case(s) contradict their declared expectation\n", failures)
		os.Exit(1)
	}
}

// verify computes the verdict for every kernel and behaviour case, writes
// a verdict line per case to w, and returns the number of cases whose
// verdict contradicts the declared expectation.
func verify(casesPath, outdir string, w io.Writer) (int, error) {
	raw, err := os.ReadFile(casesPath)
	if err != nil {
		return 0, err
	}
	var c cfg
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return 0, fmt.Errorf("parse %s: %w", casesPath, err)
	}
	failures := 0
	failures += runGroup(w, "kernel", filepath.Join(outdir, "kernel"), c.KernelCases)
	failures += runGroup(w, "behaviour", filepath.Join(outdir, "behaviour"), c.BehaviourCases)
	return failures, nil
}

func runGroup(w io.Writer, label, dir string, cases []parity.KernelCase) int {
	failures := 0
	for _, kc := range cases {
		diffs, err := parity.Compare(dir, kc.Case)
		if err != nil {
			fmt.Fprintf(w, "[%s] %-22s ERROR  %v\n", label, kc.Case, err)
			failures++
			continue
		}
		verdict, ok := parity.Judge(kc, diffs)
		if !ok {
			failures++
		}
		fmt.Fprintf(w, "[%s] %-22s %-7s %s\n", label, kc.Case, strings.ToUpper(parity.Verdict(diffs)), verdict)
	}
	return failures
}
