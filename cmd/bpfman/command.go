package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/replang"
)

// Command is the sealed interface for typed command nodes produced by
// parsing expanded REPL arguments. Each concrete variant carries
// fully resolved, validated fields ready for execution.
type Command interface {
	isCommand()
}

// ShowProgramCommand represents a fully parsed "show program" command
// with resolved program ID, view name, and output format.
type ShowProgramCommand struct {
	ID     kernel.ProgramID
	View   string
	Output OutputFlags
}

func (*ShowProgramCommand) isCommand() {}

// validShowViews lists the accepted sub-view names for "show program".
var validShowViews = map[string]bool{
	"summary": true,
	"links":   true,
	"maps":    true,
	"paths":   true,
}

// parseShowProgram resolves expanded REPL arguments into a
// ShowProgramCommand. The grammar is:
//
//	<program-id> [view] [-o format]
//
// One required positional (program ID), one optional positional (view
// name defaulting to "summary"), and one optional flag (-o).
func parseShowProgram(args []replang.Arg) (*ShowProgramCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("show program: requires a program ID")
	}

	// Resolve the program ID from the first argument.
	idStr, err := extractProgramID(args[0])
	if err != nil {
		return nil, err
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, err
	}

	cmd := &ShowProgramCommand{
		ID:   parsed.Value,
		View: "summary",
		Output: OutputFlags{
			Output: OutputValue{Value: "table"},
		},
	}

	// Walk the remaining arguments: optional view positional, optional -o flag.
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		text := argText(rest[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("show program: duplicate -o flag")
			}
			i++
			if i >= len(rest) {
				return nil, fmt.Errorf("show program: -o requires a value")
			}
			cmd.Output.Output = OutputValue{
				Value: argText(rest[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, fmt.Errorf("show program: unknown flag %q", text)
		}
		// Treat as view name.
		if !validShowViews[text] {
			return nil, fmt.Errorf("show program: unknown view %q (valid: summary, links, maps, paths)", text)
		}
		cmd.View = text
	}

	return cmd, nil
}

// execShowProgram executes a parsed ShowProgramCommand, fetching the
// program from the store and rendering output according to the
// requested view and format.
func execShowProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *ShowProgramCommand) error {
	prog, err := mgr.Get(ctx, cmd.ID)
	if err != nil {
		return err
	}

	format, err := cmd.Output.Format()
	if err != nil {
		return err
	}

	// JSON output always emits the full Program regardless of
	// sub-view; consumers can select fields with jq.
	if format == OutputFormatJSON {
		output, err := formatShowJSON(prog)
		if err != nil {
			return err
		}
		return cli.PrintOut(output)
	}

	var output string
	switch cmd.View {
	case "summary":
		var fmtErr error
		output, fmtErr = FormatProgram(prog, &cmd.Output)
		if fmtErr != nil {
			return fmtErr
		}
	case "links":
		output = formatShowLinks(prog)
	case "maps":
		output = formatShowMaps(prog)
	case "paths":
		output = formatShowPaths(prog)
	default:
		return fmt.Errorf("unknown view %q (valid: summary, links, maps, paths)", cmd.View)
	}

	return cli.PrintOut(output)
}
