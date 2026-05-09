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

// replCheckInput slurps the whole input from r, tokenises and
// parses it as one Program, runs the static checker, and
// reports every issue with a file:line: prefix. Slurping
// (rather than chunk-at-a-time) gives the checker the full
// program scope: a let in the first chunk defines a name the
// last chunk can use, and that visibility is what
// undefined-variable detection needs. Returns true when any
// error was emitted so the caller signals a non-zero exit.
func replCheckInput(r LineReader, errOut io.Writer, file string) bool {
	var b strings.Builder
	for {
		line, err := r.Readline()
		if err != nil {
			if err == io.EOF || err == ErrInterrupt {
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
	report := func(line int, msg string) {
		hadErrors = true
		loc := sourceLoc{file: file, line: line}
		fmt.Fprintf(errOut, "%serror: %s\n", loc, msg)
	}

	// stagedReport pulls a leading 'LINE:COL: ' prefix off
	// tokeniser and parser error messages so the file:line
	// rendering in report stays consistent. Without the
	// strip the user would see 'test.bpfman:1: error: 1:11:
	// unexpected ...' with the position rendered twice.
	stagedReport := func(err error) {
		msg := err.Error()
		line := 1
		if l, rest, ok := splitLineColPrefix(msg); ok {
			line = l
			msg = rest
		}
		report(line, msg)
	}

	tokens, tokErr := shell.Tokenise(src)
	if tokErr != nil {
		stagedReport(tokErr)
		return hadErrors
	}
	if len(tokens) == 0 {
		return false
	}
	prog, parseErr := shell.Parse(tokens)
	if parseErr != nil {
		stagedReport(parseErr)
		return hadErrors
	}
	for _, issue := range shell.Check(prog) {
		report(issue.Loc.Line, issue.Msg)
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

// cite returns the bare 'file:line' citation without the
// trailing ': ' separator that String adds for inline error
// prefixes. Used when the location is rendered as a value in
// its own right (e.g. captured into Job.Origin so the
// scope-exit leak diagnostic can show where the start lived).
func (l sourceLoc) cite() string {
	if l.file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", l.file, l.line)
}
