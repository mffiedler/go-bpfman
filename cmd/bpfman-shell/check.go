// --check mode: tokenise and parse each accumulated chunk of
// input, report errors with a file:line: prefix, and exit
// non-zero when any were reported.  Deliberately skips the
// session, manager, and evaluator so a broken host or empty
// database cannot mask a syntax error.  sourceLoc lives here
// too because it is conceptually a diagnostic artefact — the
// repl loop and the assertion verbs use it through the package
// surface.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

// slurpReader reads every line from r, joins them with
// newlines, and returns the resulting string. Used by the
// script-mode pre-flight (where we need the whole input
// before parsing) and by --check when invoked on stdin.
func slurpReader(r repl.LineReader) (string, error) {
	var b strings.Builder
	for {
		line, err := r.Readline()
		if err != nil {
			if err == io.EOF || err == repl.ErrInterrupt {
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

// preflightCheck tokenises and parses src, runs the static
// checker, and writes any issues to cli.Err as rust-compiler-
// style multi-line diagnostics with a "  --> file:line:col"
// citation, the offending source line, and a caret span
// underlining the region. Returns true when at least one
// issue was emitted so the caller can refuse to evaluate.
// Tokeniser, parser, and Check issues all flow as typed
// *shell.SyntaxError values; the caller pulls Span and Msg
// straight off and hands them to the renderer.
func preflightCheck(cli *bpfmancli.CLI, file, src string) bool {
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
		// Fallback: untyped error. Cite line 1 col 1 with a
		// degenerate single-column span so the renderer still
		// produces a frame; this path should be unreachable
		// once every parser/tokeniser site emits SyntaxError.
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

// runCheck drives the --check pipeline: read chunks of input, feed
// each completed chunk through Tokenise and Parse, and report the
// first error from each stage with a file:line: prefix. No Session,
// Manager, or evaluator is involved. Returns ErrSilent when any
// error was reported so the process exits non-zero without an extra
// message from Kong.
func (c *CLI) runCheck() error {
	reader, err := c.checkReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	file := c.Script
	if file == "-" || (file == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		file = "<stdin>"
	}
	if replCheckInput(reader, c.Err, file) {
		return ErrSilent
	}
	return nil
}

// checkReader chooses the input source for --check: the positional
// script file, or stdin. Unlike Run's newReader it never falls back
// to an interactive line editor because --check is a batch
// operation.
func (c *CLI) checkReader() (repl.LineReader, error) {
	if c.Script != "" {
		return openScriptReader(c.Script)
	}
	return repl.NewScannerReader(os.Stdin, nil), nil
}

// replCheckInput slurps the whole input from r, tokenises and
// parses it as one Program, runs the static checker, and
// reports every issue with a file:line: prefix. Slurping
// (rather than chunk-at-a-time) gives the checker the full
// program scope: a let in the first chunk defines a name the
// last chunk can use, and that visibility is what
// undefined-variable detection needs. Returns true when any
// error was emitted so the caller signals a non-zero exit.
func replCheckInput(r repl.LineReader, errOut io.Writer, file string) bool {
	var b strings.Builder
	for {
		line, err := r.Readline()
		if err != nil {
			if err == io.EOF || err == repl.ErrInterrupt {
				break
			}
			fmt.Fprintf(errOut, "%s: %v\n", file, err)
			return true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	src := b.String()
	if strings.TrimSpace(src) == "" {
		return false
	}

	hadErrors := false
	emitFrame := func(span shell.Span, msg string) {
		hadErrors = true
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
		return hadErrors
	}
	if len(tokens) == 0 {
		return false
	}
	prog, parseErr := shell.Parse(tokens)
	if parseErr != nil {
		reportSyntaxErr(parseErr)
		return hadErrors
	}
	for _, issue := range shell.Check(prog) {
		emitFrame(issue.Span, issue.Msg)
	}
	return hadErrors
}

// openScriptReader opens a file for reading commands. Use "-" to
// read from stdin.
func openScriptReader(path string) (repl.LineReader, error) {
	if path == "-" {
		return repl.NewScannerReader(os.Stdin, nil), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open script: %w", err)
	}
	return repl.NewScannerReader(f, f), nil
}

// sourceLoc is now repl.SourceLoc; the alias keeps existing call
// sites readable while the type definition lives in the repl
// package alongside the loop machinery that threads it through
// the dispatcher and renderer.
type sourceLoc = repl.SourceLoc
