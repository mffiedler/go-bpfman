package main

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/bpfman/bpfman/cmd/bpfman/cliformat"
)

// scalarMapper builds a Kong mapper that pops one string argument,
// labelled placeholder for error messages, and decodes it with parse.
// It is the shape shared by every single-value flag and argument mapper;
// the produced type is inferred from parse's result.
func scalarMapper[T any](placeholder string, parse func(string) (T, error)) kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto(placeholder, &s); err != nil {
			return err
		}

		v, err := parse(s)
		if err != nil {
			return err
		}

		target.Set(reflect.ValueOf(v))
		return nil
	}
}

// lowerTrimmed adapts a parser to normalise its input -- lowercased and
// whitespace-trimmed -- before parsing, matching the case-insensitive
// enum flags (program type, link kind, dispatcher type).
func lowerTrimmed[T any](parse func(string) (T, error)) func(string) (T, error) {
	return func(s string) (T, error) {
		return parse(strings.ToLower(strings.TrimSpace(s)))
	}
}

// outputValueMapper creates a Kong mapper for cliformat.OutputValue that rejects multiple -o flags.
func outputValueMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("format", &s); err != nil {
			return err
		}

		current := target.Interface().(cliformat.OutputValue)
		if current.IsSet {
			return fmt.Errorf("only one output format may be specified")
		}

		switch s {
		case string(cliformat.OutputFormatText), string(cliformat.OutputFormatJSON):
		default:
			return fmt.Errorf("invalid output format %q: must be text or json", s)
		}

		target.Set(reflect.ValueOf(cliformat.OutputValue{Value: s, IsSet: true}))
		return nil
	}
}
