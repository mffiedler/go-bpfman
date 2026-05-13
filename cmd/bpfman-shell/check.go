// --check mode: tokenise and parse the whole input, report
// errors with a file:line: prefix, and exit non-zero when any
// were reported. The pipeline itself lives in repl.CheckInput;
// the CLI methods here wire the Kong CLI struct (script path,
// stderr writer) into that primitive.
package main

import (
	"os"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
)

// runCheck drives the --check pipeline: open the input source
// and hand it to repl.CheckInput. Returns repl.ErrSilent when
// any issue was emitted so the process exits non-zero without
// an extra message from Kong.
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
	if repl.CheckInput(reader, c.Err, file) {
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

// sourceLoc is an alias for repl.SourceLoc kept for the call
// sites in ast.go and assert.go that pre-date the framework
// extraction. The type itself lives in repl/loc.go.
type sourceLoc = repl.SourceLoc
