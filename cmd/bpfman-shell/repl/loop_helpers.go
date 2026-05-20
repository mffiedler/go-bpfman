// Small loop-side helpers: the two halt-the-script sentinels,
// the chunk-continuation tracker, the history canonicaliser,
// and the captured-result failure renderer. None of them are
// big or feature-aware; the loop body that calls them will move
// to repl/ next.

package repl

import (
	"errors"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

// ErrRequireFailed is the sentinel a failed `require` chains
// into a *shell.RequireFailure so existing
// `errors.Is(err, ErrRequireFailed)` halts at the same
// script-loop boundaries that already check for it. The
// canonical value lives in the shell package; this is the
// driver-side re-export for callers that read repl import
// paths. Distinct from ErrScriptError so the loop can tell why
// the run aborted; both turn into a silent non-zero exit by the
// time Run returns to the embedding binary.
var ErrRequireFailed = shell.ErrRequireFailed

// ErrScriptError is the sentinel the loop returns when a script
// chunk emitted a runtime error that has already been printed
// with a file:line: prefix. The embedding binary translates it
// into a silent non-zero exit so Kong does not print a second
// error message.
var ErrScriptError = errors.New("script error")

// RenderEnvelopeFailure prints a captured-result failure as a
// labelled block: the verb header (guard, require, assert,
// defer), the source position of the failing statement, the
// resolved command line, the exit code, and any captured stdout
// and stderr. Empty stdout/stderr emit just the label;
// multi-line text is indented two spaces per line.
func RenderEnvelopeFailure(cli *bpfmancli.CLI, verb string, scriptLoc SourceLoc, stmtLoc shell.Pos, args []shell.Arg, env shell.Envelope) {
	file := scriptLoc.File
	if file == "" {
		file = "<repl>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] FAIL at %s:%d\n", verb, file, stmtLoc.Line)
	b.WriteString("command:\n")
	if argv := ArgTexts(args); len(argv) > 0 {
		fmt.Fprintf(&b, "  %s\n", strings.Join(argv, " "))
	}
	fmt.Fprintf(&b, "exit:\n  %d\n", env.Code)
	b.WriteString("stdout:\n")
	writeIndented(&b, env.Stdout)
	b.WriteString("stderr:\n")
	writeIndented(&b, env.Stderr)
	_ = cli.PrintErrf("%s", b.String())
}

// writeIndented appends s to b with each line prefixed by two
// spaces. A trailing newline on s is dropped before splitting so
// a captured stdout that already ended in '\n' does not produce
// a blank indented line at the end.
func writeIndented(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}
