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
)

// slurpReader, preflightCheck, and openScriptReader moved to
// repl.SlurpReader / repl.PreflightCheck / repl.OpenScriptReader.
// The CLI methods below still live here because they wire the
// Kong CLI struct into those primitives.

// runCheck drives the --check pipeline: read chunks of input, feed
// each completed chunk through Tokenise and Parse, and report the
// first error from each stage with a file:line: prefix. No Session,
// Manager, or evaluator is involved. Returns repl.ErrSilent when any
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
		return repl.ErrSilent
	}
	return nil
}

// checkReader chooses the input source for --check: the positional
// script file, or stdin. Unlike Run's newReader it never falls back
// to an interactive line editor because --check is a batch
// operation.
func (c *CLI) checkReader() (repl.LineReader, error) {
	if c.Script != "" {
		return repl.OpenScriptReader(c.Script)
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

// sourceLoc is now repl.SourceLoc; the alias keeps existing call
// sites readable while the type definition lives in the repl
// package alongside the loop machinery that threads it through
// the dispatcher and renderer.
type sourceLoc = repl.SourceLoc
