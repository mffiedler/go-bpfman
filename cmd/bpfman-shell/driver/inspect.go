// Whole-input inspection mechanism shared by the embedding CLI's
// parse-only modes. These pipelines slurp one full source unit,
// parse it as a whole program, and render either the AST or the
// canonical lowered IR without involving the manager or runtime.

package driver

import (
	"bytes"
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

// FormatInput is the framework half of the fmt pipeline: slurp the
// whole input, parse it without import expansion, and render the
// canonical source form to out. Import statements are source, not
// execution-time definitions, so formatting preserves them rather
// than splicing imported libraries into the result.
func FormatInput(r LineReader, out io.Writer, errOut io.Writer, file string) bool {
	formatted, hadIssue := FormatInputString(r, errOut, file)
	if hadIssue {
		return true
	}
	_, err := io.WriteString(out, formatted)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", file, err)
		return true
	}
	return false
}

// FormatInputString is FormatInput's string-returning form, used by
// the CLI's write-back mode.
func FormatInputString(r LineReader, errOut io.Writer, file string) (string, bool) {
	src, err := SlurpReader(r)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", file, err)
		return "", true
	}
	if strings.TrimSpace(src) == "" {
		return "", false
	}
	reportErr := func(err error) (string, bool) {
		loc := SourceLoc{File: file, Line: 1}
		fmt.Fprintf(errOut, "%serror: %v\n", loc, err)
		return "", true
	}
	prog, parseErr := parseProgram(file, src)
	if parseErr != nil {
		return reportErr(parseErr)
	}
	originalLowered, err := loweredDump(prog)
	if err != nil {
		return reportErr(err)
	}
	formatted := syntax.FormatSource(src, prog)
	formattedProg, parseErr := parseProgram(file, formatted)
	if parseErr != nil {
		return reportErr(parseErr)
	}
	formattedLowered, err := loweredDump(formattedProg)
	if err != nil {
		return reportErr(err)
	}
	if originalLowered != formattedLowered {
		return reportErr(fmt.Errorf("internal formatter error: formatted source changed lowered form"))
	}
	return formatted, false
}

func loweredDump(prog *syntax.Program) (string, error) {
	lp, err := lower.Lower(prog)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := ir.Dump(&out, lp); err != nil {
		return "", err
	}
	return out.String(), nil
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
