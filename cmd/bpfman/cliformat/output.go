package cliformat

import (
	"fmt"
)

// OutputFormat represents the output format type.
type OutputFormat string

// The supported output formats.
const (
	OutputFormatText OutputFormat = "text"
	OutputFormatJSON OutputFormat = "json"
)

// OutputValue wraps an output format string and tracks whether it has been set.
// This allows detection of multiple -o flags, which is an error.
type OutputValue struct {
	// Value is the requested format string, normally "text" or "json".
	Value string

	// IsSet reports whether the -o flag was supplied, used to reject a repeated flag.
	IsSet bool
}

// OutputFlags provides output formatting flags.
type OutputFlags struct {
	// Output selects the output format (text or json) via the -o/--output flag; it defaults to text.
	Output OutputValue `short:"o" help:"Output format: text, json." default:"text"`
}

// Format returns the base format type, or an error if the format is unrecognised.
func (f *OutputFlags) Format() (OutputFormat, error) {
	v := f.Output.Value
	switch {
	case v == "text":
		return OutputFormatText, nil
	case v == "json":
		return OutputFormatJSON, nil
	default:
		return "", fmt.Errorf("unknown output format %q; valid formats: text, json", v)
	}
}

// IsStructured reports whether the output format should produce valid
// output even when the result set is empty.
func (f OutputFormat) IsStructured() bool {
	return f == OutputFormatJSON
}

// NeedsLinkGetProgramName reports whether get-link output renders the
// presentation-only BPF Function row.
func (f OutputFormat) NeedsLinkGetProgramName() bool {
	return f == OutputFormatText
}
