// --ast mode: tokenise and parse each accumulated chunk of
// input, print the resulting AST tree to stdout, and exit.
// Sibling of --check: same chunk-accumulation logic, same
// 'no manager / session / evaluator' isolation. The dumper is
// reflective so adding a new statement or expression variant
// to the shell package surfaces in --ast output without code
// changes here.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/shell"
)

// runAST drives the --ast pipeline: read chunks of input,
// parse each completed chunk, dump the resulting Program to
// stdout. Errors are reported with a file:line: prefix and
// the process exits non-zero (via ErrSilent) when any chunk
// failed to parse, matching --check's diagnostic shape.
func (c *CLI) runAST() error {
	reader, err := c.checkReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	file := c.Script
	if file == "-" || (file == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		file = "<stdin>"
	}
	if replASTInput(reader, c.Out, c.Err, file) {
		return ErrSilent
	}
	return nil
}

// replASTInput reads from r, accumulates lines until brace and
// bracket depth balances (mirroring replLoop), tokenises and
// parses each accumulated chunk, and writes the dumped AST to
// out. Parse errors are written to errOut with a file:line:
// prefix; returns true when any error was emitted.
func replASTInput(r LineReader, out io.Writer, errOut io.Writer, file string) bool {
	var lineNo int
	var buf strings.Builder
	var startLine int
	var cs contState
	hadErrors := false

	reportErr := func(line int, err error) {
		hadErrors = true
		loc := sourceLoc{file: file, line: line}
		fmt.Fprintf(errOut, "%serror: %v\n", loc, err)
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
		prog, parseErr := shell.Parse(tokens)
		if parseErr != nil {
			reportErr(startLine, parseErr)
			continue
		}
		if dumpErr := shell.DumpAST(out, prog); dumpErr != nil {
			reportErr(startLine, dumpErr)
		}
	}
	return hadErrors
}
