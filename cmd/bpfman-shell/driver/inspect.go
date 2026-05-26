// Whole-input inspection mechanism shared by the embedding CLI's
// parse-only modes. These pipelines slurp one full source unit,
// parse it as a whole program, and render either the AST or the
// canonical lowered IR without involving the manager or runtime.

package driver

import (
	"fmt"
	"io"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/ir"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/lower"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
)

// ASTInput is the framework half of the --ast pipeline: slurp the
// whole input from r, parse it as one program, and dump the AST to
// out. Errors are written to errOut with a file:line prefix; returns
// true when any error was emitted so the embedding CLI can exit
// non-zero without printing a second message.
func ASTInput(r LineReader, out io.Writer, errOut io.Writer, file string) bool {
	return renderWholeProgramInput(r, out, errOut, file, func(out io.Writer, prog *syntax.Program) error {
		return syntax.DumpAST(out, prog)
	})
}

// LoweredInput is the framework half of the --lowered pipeline:
// slurp the whole input from r, parse it as one program, lower it to
// the canonical IR, and dump the lowered form to out. Errors are
// written to errOut with a file:line prefix; returns true when any
// error was emitted so the embedding CLI can exit non-zero without
// printing a second message.
func LoweredInput(r LineReader, out io.Writer, errOut io.Writer, file string) bool {
	return renderWholeProgramInput(r, out, errOut, file, func(out io.Writer, prog *syntax.Program) error {
		lp, err := lower.Lower(prog)
		if err != nil {
			return err
		}
		return ir.Dump(out, lp)
	})
}

func renderWholeProgramInput(r LineReader, out io.Writer, errOut io.Writer, file string, render func(io.Writer, *syntax.Program) error) bool {
	src, err := SlurpReader(r)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", file, err)
		return true
	}
	if strings.TrimSpace(src) == "" {
		return false
	}

	reportErr := func(err error) bool {
		loc := SourceLoc{File: file, Line: 1}
		fmt.Fprintf(errOut, "%serror: %v\n", loc, err)
		return true
	}

	prog, parseErr := ParseAndExpand(file, src)
	if parseErr != nil {
		return reportErr(parseErr)
	}
	if renderErr := render(out, prog); renderErr != nil {
		return reportErr(renderErr)
	}
	return false
}
