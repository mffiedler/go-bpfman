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
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/shell"
)

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
func (c *CLI) checkReader() (LineReader, error) {
	if c.Script != "" {
		return openScriptReader(c.Script)
	}
	return NewScannerReader(os.Stdin, nil), nil
}

// replCheckInput reads from r, accumulates lines until brace and
// bracket depth balances (mirroring replLoop), and checks each
// accumulated chunk via shell.Tokenise and shell.Parse. Errors are
// written to errOut with a file:line: prefix. Returns true when any
// error was emitted so the caller can signal a non-zero exit.
func replCheckInput(r LineReader, errOut io.Writer, file string) bool {
	var lineNo int
	var buf strings.Builder
	var startLine int
	var cs contState
	hadErrors := false

	reportErr := func(line int, err error) {
		hadErrors = true
		loc := sourceLoc{file: file, line: line}
		fmt.Fprintf(errOut, "%s[check] error: %v\n", loc, err)
	}

	for {
		input, err := r.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				if buf.Len() > 0 {
					reportErr(startLine, fmt.Errorf("unterminated block at end of input"))
				}
				break
			}
			reportErr(lineNo, err)
			break
		}
		lineNo++

		if buf.Len() == 0 {
			startLine = lineNo
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(input)
		cs.advance(input)
		if cs.open() {
			continue
		}

		accumulated := buf.String()
		buf.Reset()
		cs = contState{}

		tokens, tokErr := shell.Tokenise(accumulated)
		if tokErr != nil {
			reportErr(startLine, tokErr)
			continue
		}
		if len(tokens) == 0 {
			continue
		}
		if _, parseErr := shell.Parse(tokens); parseErr != nil {
			reportErr(startLine, parseErr)
		}
	}
	return hadErrors
}

// openScriptReader opens a file for reading commands. Use "-" to
// read from stdin.
func openScriptReader(path string) (LineReader, error) {
	if path == "-" {
		return NewScannerReader(os.Stdin, nil), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open script: %w", err)
	}
	return NewScannerReader(f, f), nil
}

// sourceLoc identifies a position in a script file. The zero value
// means "no location" and formats as the empty string, so interactive
// and piped-stdin modes are unaffected.
type sourceLoc struct {
	file string
	line int
}

func (l sourceLoc) String() string {
	if l.file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d: ", l.file, l.line)
}
