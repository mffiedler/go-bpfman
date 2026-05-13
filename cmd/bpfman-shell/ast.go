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

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// runAST drives the --ast pipeline: slurp the whole input,
// tokenise and parse it as a single program, and dump the
// resulting tree to stdout. Slurping (rather than the chunk-
// at-a-time shape used by --check and the evaluator) is
// deliberate: AST inspection wants one tree for the whole
// file, not one tree per balanced statement. Errors are
// reported with a file:line: prefix and the process exits
// non-zero (via ErrSilent) when parsing fails.
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

// replASTInput reads every line from r, joins them with
// newlines, tokenises the whole input as one piece, parses
// it as one Program, and dumps the resulting tree to out.
// Parse errors are written to errOut with a file:line:
// prefix; returns true when any error was emitted. Empty
// input produces empty output and no error.
func replASTInput(r repl.LineReader, out io.Writer, errOut io.Writer, file string) bool {
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

	reportErr := func(err error) bool {
		loc := sourceLoc{file: file, line: 1}
		fmt.Fprintf(errOut, "%serror: %v\n", loc, err)
		return true
	}

	tokens, tokErr := shell.Tokenise(src)
	if tokErr != nil {
		return reportErr(tokErr)
	}
	if len(tokens) == 0 {
		return false
	}
	prog, parseErr := shell.Parse(tokens)
	if parseErr != nil {
		return reportErr(parseErr)
	}
	if dumpErr := shell.DumpAST(out, prog); dumpErr != nil {
		return reportErr(dumpErr)
	}
	return false
}
