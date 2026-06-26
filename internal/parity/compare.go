// Package parity compares the kernel + bpffs footprints captured by
// bpftool for the Go and Rust implementations of a parity case, and
// judges the result against a declared expectation. bpftool is the
// neutral juror; this package only diffs what it recorded. It is shared
// by cmd/parity-compare (the gate) and cmd/parity-readme (the rendered
// support table) so both compute the verdict the same way.
package parity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// KernelCase is one entry under cases.yaml's kernel_cases: a case name
// and the expected outcome ("match", or "differ" only at the paths in
// On).
type KernelCase struct {
	Case   string   `yaml:"case"`
	Expect string   `yaml:"expect"`
	On     []string `yaml:"on"`
}

// Compare diffs the Go and Rust kernel footprints for name under dir
// (<name>.go.json vs <name>.rust.json) and returns one entry per leaf
// path that differs, each "<path> <go> vs <rust>", sorted.
func Compare(dir, name string) ([]string, error) {
	g, err := readJSON(filepath.Join(dir, name+".go.json"))
	if err != nil {
		return nil, err
	}
	r, err := readJSON(filepath.Join(dir, name+".rust.json"))
	if err != nil {
		return nil, err
	}
	var diffs []string
	deepDiff(g, r, "", &diffs)
	sort.Strings(diffs)
	return diffs, nil
}

// Verdict is "match" if diffs is empty, else "differ".
func Verdict(diffs []string) string {
	if len(diffs) == 0 {
		return "match"
	}
	return "differ"
}

// Judge reports a human verdict for kc given the computed diffs, and
// whether the computed outcome holds the declared expectation.
func Judge(kc KernelCase, diffs []string) (string, bool) {
	switch kc.Expect {
	case "match":
		if len(diffs) == 0 {
			return "kernel state matches", true
		}
		return "EXPECTED match, but kernel state differs at: " + strings.Join(diffs, "; "), false
	case "differ":
		want := append([]string(nil), kc.On...)
		sort.Strings(want)
		got := Paths(diffs)
		sort.Strings(got)
		if len(diffs) == 0 {
			return "EXPECTED a difference at " + strings.Join(want, ",") + ", but kernel state matches (has Rust changed?)", false
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return "EXPECTED difference only at " + strings.Join(want, ",") + ", but found: " + strings.Join(diffs, "; "), false
		}
		return "differs only at " + strings.Join(want, ",") + " (as expected): " + strings.Join(diffs, "; "), true
	default:
		return "unknown expect " + kc.Expect, false
	}
}

// Paths strips the value suffix from each diff entry, leaving the field
// path.
func Paths(diffs []string) []string {
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		if i := strings.IndexByte(d, ' '); i > 0 {
			out = append(out, d[:i])
		} else {
			out = append(out, d)
		}
	}
	return out
}

func readJSON(path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// deepDiff records into out every leaf path where a (Go) and b (Rust)
// differ, each "<path> <go-value> vs <rust-value>".
func deepDiff(a, b any, path string, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s type (object vs %T)", path, b))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			cp := k
			if path != "" {
				cp = path + "." + k
			}
			ae, aok := av[k]
			be, bok := bv[k]
			switch {
			case aok && !bok:
				*out = append(*out, fmt.Sprintf("%s present in Go only", cp))
			case !aok && bok:
				*out = append(*out, fmt.Sprintf("%s present in Rust only", cp))
			default:
				deepDiff(ae, be, cp, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s type (array vs %T)", path, b))
			return
		}
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s length (%d vs %d)", path, len(av), len(bv)))
			return
		}
		for i := range av {
			deepDiff(av[i], bv[i], fmt.Sprintf("%s[%d]", path, i), out)
		}
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			*out = append(*out, fmt.Sprintf("%s %v vs %v", path, a, b))
		}
	}
}
