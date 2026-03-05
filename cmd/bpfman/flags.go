package main

import (
	"errors"
	"fmt"
	"time"
)

// ErrSilent is returned when the error has already been communicated
// (e.g., via JSON output) and cli.go should exit non-zero without
// printing an additional error message.
var ErrSilent = errors.New("silent error")

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
	OutputFormatTable    OutputFormat = "table"
	OutputFormatTree     OutputFormat = "tree"
	OutputFormatJSON     OutputFormat = "json"
	OutputFormatJSONPath OutputFormat = "jsonpath"
	OutputFormatWide     OutputFormat = "wide"
)

// OutputValue wraps an output format string and tracks whether it has been set.
// This allows detection of multiple -o flags, which is an error.
type OutputValue struct {
	Value string
	IsSet bool
}

// OutputFlags provides output formatting flags.
type OutputFlags struct {
	Output OutputValue `short:"o" help:"Output format: table, wide, json, tree, jsonpath=EXPR." default:"table"`
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
	default:
		return "", fmt.Errorf("unknown output format %q; valid formats: table, wide, json, tree, jsonpath=EXPR", v)
	}
}

// IsStructured reports whether the output format is a structured
// format (JSON or JSONPath) that should produce valid output even when
// the result set is empty.
func (f *OutputFlags) IsStructured() bool {
	format, err := f.Format()
	if err != nil {
		return false
	}
	return format == OutputFormatJSON || format == OutputFormatJSONPath
}

// JSONPathExpr returns the JSONPath expression if format is jsonpath=EXPR.
func (f *OutputFlags) JSONPathExpr() string {
	v := f.Output.Value
	if len(v) > 9 && v[:9] == "jsonpath=" {
		return v[9:]
	}
	return ""
}

// TTLFlag provides a TTL duration flag.
type TTLFlag struct {
	TTL time.Duration `help:"Time-to-live duration." default:"5m"`
}
