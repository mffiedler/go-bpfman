// Input-side mechanism: open a script file or stdin as a
// LineReader, slurp the whole input, and run the static-check
// pre-flight. The loop calls these directly; the --check and
// --ast pipelines in the embedding binary also reuse them.

package repl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// OpenScriptReader opens a file for reading commands. Use "-"
// to read from stdin.
func OpenScriptReader(path string) (LineReader, error) {
	if path == "-" {
		return NewScannerReader(os.Stdin, nil), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open script: %w", err)
	}
	return NewScannerReader(f, f), nil
}

// SlurpReader reads every line from r, joins them with newlines,
// and returns the resulting string. Used by the script-mode
// pre-flight (where we need the whole input before parsing) and
// by --check when invoked on stdin.
func SlurpReader(r LineReader) (string, error) {
	var b strings.Builder
	for {
		line, err := r.Readline()
		if err != nil {
			if err == io.EOF || err == ErrInterrupt {
				return b.String(), nil
			}
			return "", err
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
}

// PreflightCheck tokenises and parses src, runs the static
// checker, and writes any issues to errOut as rust-compiler-
// style multi-line diagnostics with a "  --> file:line:col"
// citation, the offending source line, and a caret span
// underlining the region. Returns true when at least one issue
// was emitted so the caller can refuse to evaluate.
func PreflightCheck(errOut io.Writer, file, src string) bool {
	if strings.TrimSpace(src) == "" {
		return false
	}
	hadIssues := false
	emitFrame := func(span shell.Span, msg string) {
		hadIssues = true
		fmt.Fprint(errOut, shell.RenderDiagnostic(src, file, shell.Diagnostic{
			Span: span,
			Msg:  msg,
		}))
	}
	reportSyntaxErr := func(err error) {
		var se *shell.SyntaxError
		if errors.As(err, &se) {
			emitFrame(se.Span, se.Msg)
			return
		}
		emitFrame(shell.Span{
			Pos: shell.Pos{Line: 1, Col: 1},
			End: shell.Pos{Line: 1, Col: 2},
		}, err.Error())
	}

	tokens, tokErr := shell.Tokenise(src)
	if tokErr != nil {
		reportSyntaxErr(tokErr)
		return hadIssues
	}
	if len(tokens) == 0 {
		return false
	}
	prog, parseErr := shell.Parse(tokens)
	if parseErr != nil {
		reportSyntaxErr(parseErr)
		return hadIssues
	}
	for _, issue := range shell.Check(prog) {
		emitFrame(issue.Span, issue.Msg)
	}
	return hadIssues
}

// CheckInput is the framework half of the --check pipeline:
// slurp the whole input from r, then run PreflightCheck on the
// concatenated source. Returns true when at least one issue was
// emitted so the caller can signal a non-zero exit. Slurping
// (rather than chunk-at-a-time) gives the checker the full
// program scope: a let in the first chunk defines a name the
// last chunk can use, and that visibility is what undefined-
// variable detection needs.
func CheckInput(r LineReader, errOut io.Writer, file string) bool {
	src, err := SlurpReader(r)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", file, err)
		return true
	}
	return PreflightCheck(errOut, file, src)
}
