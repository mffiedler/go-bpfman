package cli

import (
	"errors"
	"fmt"
	"time"
)

// ErrSilent is returned when the error has already been communicated
// (e.g., via JSON output) and cli.go should exit non-zero without
// printing an additional error message.
var ErrSilent = errors.New("silent error")

// DryRunFlag provides a --dry-run flag for commands that support it.
type DryRunFlag struct {
	DryRun bool `help:"Show what would be done without making changes."`
}

// MetadataFlags provides metadata-related flags.
type MetadataFlags struct {
	Metadata []KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata to attach (can be repeated)."`
}

// GlobalDataFlags provides global data flags.
type GlobalDataFlags struct {
	GlobalData []GlobalData `short:"g" name:"global" help:"NAME=HEX global data (can be repeated)."`
}

// OutputFormat represents the output format type.
type OutputFormat string

const (
	OutputFormatTable             OutputFormat = "table"
	OutputFormatTree              OutputFormat = "tree"
	OutputFormatJSON              OutputFormat = "json"
	OutputFormatJSONPath          OutputFormat = "jsonpath"
	OutputFormatWide              OutputFormat = "wide"
	OutputFormatCustomColumns     OutputFormat = "custom-columns"
	OutputFormatCustomColumnsFile OutputFormat = "custom-columns-file"
)

// OutputFlags provides output formatting flags.
type OutputFlags struct {
	Output string `short:"o" help:"Output format: table, wide, json, tree, jsonpath=EXPR, custom-columns=SPEC, custom-columns-file=FILE." default:"table"`
}

// Format returns the base format type, or an error if the format is unrecognised.
func (f *OutputFlags) Format() (OutputFormat, error) {
	switch {
	case f.Output == "table":
		return OutputFormatTable, nil
	case f.Output == "wide":
		return OutputFormatWide, nil
	case f.Output == "tree":
		return OutputFormatTree, nil
	case f.Output == "json":
		return OutputFormatJSON, nil
	case len(f.Output) > 9 && f.Output[:9] == "jsonpath=":
		return OutputFormatJSONPath, nil
	case len(f.Output) > 15 && f.Output[:15] == "custom-columns=":
		return OutputFormatCustomColumns, nil
	case len(f.Output) > 20 && f.Output[:20] == "custom-columns-file=":
		return OutputFormatCustomColumnsFile, nil
	default:
		return "", fmt.Errorf("unknown output format %q; valid formats: table, wide, json, tree, jsonpath=EXPR, custom-columns=SPEC, custom-columns-file=FILE", f.Output)
	}
}

// JSONPathExpr returns the JSONPath expression if format is jsonpath=EXPR.
func (f *OutputFlags) JSONPathExpr() string {
	if len(f.Output) > 9 && f.Output[:9] == "jsonpath=" {
		return f.Output[9:]
	}
	return ""
}

// CustomColumnsSpec returns the custom-columns spec if format is custom-columns=SPEC.
func (f *OutputFlags) CustomColumnsSpec() string {
	if len(f.Output) > 15 && f.Output[:15] == "custom-columns=" {
		return f.Output[15:]
	}
	return ""
}

// CustomColumnsFile returns the file path if format is custom-columns-file=FILE.
func (f *OutputFlags) CustomColumnsFile() string {
	if len(f.Output) > 20 && f.Output[:20] == "custom-columns-file=" {
		return f.Output[20:]
	}
	return ""
}

// TTLFlag provides a TTL duration flag.
type TTLFlag struct {
	TTL time.Duration `help:"Time-to-live duration." default:"5m"`
}
