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
	"github.com/frobware/go-bpfman/internal/bpfmancli"
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
// checker, and writes any issues to cli.Err as rust-compiler-
// style multi-line diagnostics with a "  --> file:line:col"
// citation, the offending source line, and a caret span
// underlining the region. Returns true when at least one issue
// was emitted so the caller can refuse to evaluate.
func PreflightCheck(cli *bpfmancli.CLI, file, src string) bool {
	if strings.TrimSpace(src) == "" {
		return false
	}
	hadIssues := false
	emitFrame := func(span shell.Span, msg string) {
		hadIssues = true
		_ = cli.PrintErr(shell.RenderDiagnostic(src, file, shell.Diagnostic{
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
