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

// OutputValue wraps an output format string and tracks whether it has been set.
// This allows detection of multiple -o flags, which is an error.
type OutputValue struct {
	Value string
	IsSet bool
}

// OutputFlags provides output formatting flags.
type OutputFlags struct {
	Output OutputValue `short:"o" help:"Output format: table, wide, json, tree, jsonpath=EXPR, custom-columns=SPEC, custom-columns-file=FILE." default:"table"`
}

// Format returns the base format type, or an error if the format is unrecognised.
func (f *OutputFlags) Format() (OutputFormat, error) {
	v := f.Output.Value
	switch {
	case v == "table":
		return OutputFormatTable, nil
	case v == "wide":
		return OutputFormatWide, nil
	case v == "tree":
		return OutputFormatTree, nil
	case v == "json":
		return OutputFormatJSON, nil
	case len(v) > 9 && v[:9] == "jsonpath=":
		return OutputFormatJSONPath, nil
	case len(v) > 15 && v[:15] == "custom-columns=":
		return OutputFormatCustomColumns, nil
	case len(v) > 20 && v[:20] == "custom-columns-file=":
		return OutputFormatCustomColumnsFile, nil
	default:
		return "", fmt.Errorf("unknown output format %q; valid formats: table, wide, json, tree, jsonpath=EXPR, custom-columns=SPEC, custom-columns-file=FILE", v)
	}
}

// JSONPathExpr returns the JSONPath expression if format is jsonpath=EXPR.
func (f *OutputFlags) JSONPathExpr() string {
	v := f.Output.Value
	if len(v) > 9 && v[:9] == "jsonpath=" {
		return v[9:]
	}
	return ""
}

// CustomColumnsSpec returns the custom-columns spec if format is custom-columns=SPEC.
func (f *OutputFlags) CustomColumnsSpec() string {
	v := f.Output.Value
	if len(v) > 15 && v[:15] == "custom-columns=" {
		return v[15:]
	}
	return ""
}

// CustomColumnsFile returns the file path if format is custom-columns-file=FILE.
func (f *OutputFlags) CustomColumnsFile() string {
	v := f.Output.Value
	if len(v) > 20 && v[:20] == "custom-columns-file=" {
		return v[20:]
	}
	return ""
}

// TTLFlag provides a TTL duration flag.
type TTLFlag struct {
	TTL time.Duration `help:"Time-to-live duration." default:"5m"`
}
