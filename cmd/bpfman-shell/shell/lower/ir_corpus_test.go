package lower

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebdah/goldie/v2"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/ir"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
)

// corpusEntry names a directory of scripts to snapshot. Label
// is the subtree under testdata/lowered/ where this entry's
// goldens live; Source is the directory containing the .bpfman
// files, expressed relative to this test file.
type corpusEntry struct {
	Label  string
	Source string
}

// TestLoweredCorpus parses, lowers, and dumps every script in
// the listed corpora and asserts the dump matches a checked-in
// golden under lower/testdata/lowered/. Regenerate with the repo-level
// target that forces a fresh test run with goldie updates
// enabled:
//
//	direnv exec . make update-lowered-corpus
//
// The target bakes in `-count=1`: without it `go test` may
// satisfy the run from cache before goldie rewrites the
// fixtures. A regeneration run with no source changes must
// produce no on-disk diff; that is the snapshot contract.
func TestLoweredCorpus(t *testing.T) {
	t.Parallel()
	entries := []corpusEntry{
		{Label: "e2e/scripts", Source: "../../../../e2e/scripts"},
		{Label: "e2e/old", Source: "../../../../e2e/old"},
		{Label: "e2e/lib", Source: "../../../../e2e"},
	}
	for _, e := range entries {
		t.Run(e.Label, func(t *testing.T) {
			t.Parallel()
			runCorpus(t, e)
		})
	}
}

// runCorpus snapshots one corpus entry, using a goldie instance
// scoped to that entry's subtree under testdata/lowered/.
func runCorpus(t *testing.T, e corpusEntry) {
	t.Helper()
	files, err := corpusFiles(e)
	if err != nil {
		t.Fatalf("collect %s: %v", e.Label, err)
	}
	if len(files) == 0 {
		t.Fatalf("no scripts found under %s", e.Source)
	}
	g := goldie.New(t,
		goldie.WithFixtureDir(filepath.Join("testdata", "lowered", e.Label)),
		goldie.WithNameSuffix(".lowered"),
	)
	for _, src := range files {
		name := strings.TrimSuffix(filepath.Base(src), ".bpfman")
		t.Run(name, func(t *testing.T) {
			actual, err := lowerToText(src)
			if err != nil {
				t.Fatalf("lower %s: %v", src, err)
			}
			g.Assert(t, name, []byte(actual))
		})
	}
}

// corpusFiles returns the .bpfman files belonging to one
// corpus entry. The "e2e/lib" entry is the single top-level
// lib.bpfman; the other entries pull every *.bpfman in their
// directory.
func corpusFiles(e corpusEntry) ([]string, error) {
	if e.Label == "e2e/lib" {
		path := filepath.Join(e.Source, "lib.bpfman")
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		return []string{path}, nil
	}
	pattern := filepath.Join(e.Source, "*.bpfman")
	return filepath.Glob(pattern)
}

// lowerToText reads the script at path, tokenises, parses, and
// lowers it, returning the dump as a string.
func lowerToText(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tokens, err := syntax.Tokenise(string(src))
	if err != nil {
		return "", err
	}
	prog, err := syntax.Parse(tokens)
	if err != nil {
		return "", err
	}
	lp, err := Lower(prog)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := ir.Dump(&buf, lp); err != nil {
		return "", err
	}
	return buf.String(), nil
}
