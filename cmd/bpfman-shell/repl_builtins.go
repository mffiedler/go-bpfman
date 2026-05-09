// Shell-language built-in commands: jq, file temp, exec.  They do
// not touch the bpfman manager; they operate on values, the
// filesystem, and the subprocess table respectively.  Each has a
// small coherent job and lives here so repl.go can stay focused on
// the read / eval / dispatch loop.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/shell"
)

// firstFlagArg returns the text of the first "-x" or "--long"
// argument in args, or ("", false) if none appear.  Used to craft
// specific error messages when a user passes shell-jq-style flags
// to the filter-only replJQ.
func firstFlagArg(args []shell.Arg) (string, bool) {
	for _, a := range args {
		text := argText(a)
		if len(text) >= 2 && text[0] == '-' && text[1] != '0' && (text[1] < '0' || text[1] > '9') && text[1] != '.' {
			return text, true
		}
	}
	return "", false
}

// replJQ runs a jq filter against a Value using an embedded gojq
// interpreter.  It is the DSL's "higher-order ops over JSON-shaped
// data" primitive.  Shape: jq <filter> <value>.
//
// The filter is a scalar (Word/Quoted/ScalarValue); the value may
// be scalar or structured.  Multiple jq results are collected into
// a list Value; zero results yield a nil Value; a single result is
// returned directly.  Integer outputs from gojq are normalised to
// json.Number so downstream Scalar() and path access treat them
// like any other numeric value in the pipeline.  Bool results get
// OriginBool so AsBool works on them for assertions.
func replJQ(args []shell.Arg) (shell.Value, error) {
	if len(args) != 2 {
		// Users reaching for standalone jq reflexively include
		// output-formatting flags (-c, -r, --tab).  Ours is
		// filter-only — rendering is done by the consumer
		// (compact via "${...}" interpolation, indented via the
		// REPL's auto-print).  Surface that explicitly so the
		// user is not left guessing why -c was rejected.
		if flag, ok := firstFlagArg(args); ok {
			return shell.Value{}, fmt.Errorf("usage: jq <filter> <value>; our jq is filter-only (no %q flag); use \"${expr}\" for compact JSON or run the real jq via [exec jq ...]", flag)
		}
		return shell.Value{}, fmt.Errorf("usage: jq <filter> <value>")
	}
	filterText := argText(args[0])
	query, err := gojq.Parse(filterText)
	if err != nil {
		return shell.Value{}, fmt.Errorf("jq: parse filter: %w", err)
	}
	input, err := argToJQInput(args[1])
	if err != nil {
		return shell.Value{}, fmt.Errorf("jq: %w", err)
	}

	iter := query.Run(input)
	var results []any
	for {
		v, hasMore := iter.Next()
		if !hasMore {
			break
		}
		if iterErr, ok := v.(error); ok {
			return shell.Value{}, fmt.Errorf("jq: %w", iterErr)
		}
		results = append(results, normaliseJQValue(v))
	}
	switch len(results) {
	case 0:
		return shell.Value{}, nil
	case 1:
		return wrapJQResult(results[0]), nil
	default:
		return wrapJQResult(results), nil
	}
}

// argToJQInput extracts a JSON-compatible any from a shell.Arg.
// Structured args pass through as their Raw representation;
// scalar args are parsed as JSON text, matching the default
// behaviour of the standalone jq CLI (which reads stdin as
// JSON).  A scalar that isn't valid JSON is an error — users who
// want to pass a literal string wrap it in JSON quotes
// ('"hello"' rather than 'hello').
func argToJQInput(a shell.Arg) (any, error) {
	switch v := a.(type) {
	case shell.WordArg:
		return decodeJQScalar(v.Text)
	case shell.QuotedArg:
		return decodeJQScalar(v.Text)
	case shell.ScalarValueArg:
		return decodeJQScalar(v.Text)
	case shell.StructuredValueArg:
		return v.Value.Raw(), nil
	case shell.AdapterArg:
		return v.Value.Raw(), nil
	default:
		return nil, fmt.Errorf("unsupported input type %T", a)
	}
}

// decodeJQScalar parses a scalar as a single JSON value.  Numbers
// come back as json.Number so Value.Scalar() renders them
// losslessly; trailing data after the value is rejected so
// sloppy inputs fail fast.
func decodeJQScalar(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("input is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("input is not valid JSON: trailing data after value")
	}
	return v, nil
}

// normaliseJQValue walks a jq output and converts Go-native
// integer types to json.Number so the result lines up with the
// rest of the pipeline, which carries numbers as json.Number
// throughout.  float64 is left alone; nested maps and slices are
// rewritten recursively.
func normaliseJQValue(x any) any {
	switch v := x.(type) {
	case int:
		return json.Number(strconv.Itoa(v))
	case int64:
		return json.Number(strconv.FormatInt(v, 10))
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = normaliseJQValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = normaliseJQValue(e)
		}
		return out
	default:
		return v
	}
}

// wrapJQResult turns a single jq output into a shell.Value,
// preferring the most specific origin kind so assertions and
// other origin-aware consumers see the shape they expect: bool →
// OriginBool, scalar → OriginScalar, structured → OriginUnknown.
func wrapJQResult(x any) shell.Value {
	if x == nil {
		// jq produced an explicit null (for example, ".name"
		// against an object without a name field).  Return a
		// present null rather than a zero Value so downstream
		// substitution, assignment, interpolation, and
		// comparisons treat it as a real value.
		return shell.NullValue()
	}
	if b, ok := x.(bool); ok {
		return shell.BoolValue(b)
	}
	v := shell.ValueFromAny(x)
	if v.IsScalar() {
		return v.WithKind(shell.OriginScalar)
	}
	return v
}

// replFile implements the file shell command. The only subcommand is
// "temp", which writes a REPL value to a private temporary file and
// returns the path as a scalar string. The single argument is any
// value-producing expression: a variable reference ($var, $var.path),
// a command substitution ([...]), or a quoted literal. A bare word
// is treated as a literal string -- to write the contents of a
// variable, pass $name, not the bare name.
func replFile(cli *bpfmancli.CLI, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 || argText(args[0]) != "temp" {
		return shell.Value{}, fmt.Errorf("usage: file temp $var[.path] | [expr] | \"literal\"")
	}
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("file temp requires exactly one argument")
	}
	v, err := printValue(args[1])
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	path, err := writeValueToTemp(v)
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	if err := cli.PrintOut(path + "\n"); err != nil {
		return shell.Value{}, err
	}
	return shell.StringValue(path), nil
}

// writeValueToTemp renders a shell.Value to a private temporary file
// and returns the absolute path. The file is created with mode 0600
// in the OS default temp directory with a recognisable prefix.
func writeValueToTemp(v shell.Value) (string, error) {
	data, err := shell.RenderValue(v)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "bpfman-repl-")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	return f.Name(), nil
}

// execResult holds the captured output of a subprocess run by the
// exec shell command. The JSON tags produce the field names visible
// in the REPL's structured-value model.
type execResult struct {
	Argv     []string `json:"argv"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit_code"`
}

// execCapture is the result of running an external command without
// any policy applied: argv as constructed, captured stdout and
// stderr, and the actual exit code. Launch failure (command not
// found, permission denied) is reported as a Go error from
// runExternal and never appears as an execCapture.
type execCapture struct {
	Argv     []string
	Stdout   string
	Stderr   string
	ExitCode int
}

// runExternal runs an external command and captures its output.
// Inline adapter arguments (e.g. file:$var.path) are resolved to
// temporary files before the command runs and removed
// unconditionally after. Structured-value arguments are rejected
// because they cannot be flattened into argv text. Non-zero exit
// is reported via execCapture.ExitCode, not as an error: callers
// decide whether non-zero is fatal.
func runExternal(ctx context.Context, args []shell.Arg) (execCapture, error) {
	if len(args) == 0 {
		return execCapture{}, fmt.Errorf("exec requires at least one argument")
	}

	var tempFiles []string
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		switch aa := a.(type) {
		case shell.AdapterArg:
			if aa.Adapter != "file" {
				return execCapture{}, fmt.Errorf("unknown adapter %q", aa.Adapter)
			}
			path, err := writeValueToTemp(aa.Value)
			if err != nil {
				return execCapture{}, fmt.Errorf("adapter file: %w", err)
			}
			tempFiles = append(tempFiles, path)
			resolved[i] = shell.ScalarValueArg{Text: path}
		case shell.StructuredValueArg:
			// A structured value (program, link, exec.result,
			// envelope, ...) cannot be flattened into argv text.
			// Access a scalar field (e.g. $result.stdout) or use
			// the file adapter (file:$result).
			return execCapture{}, fmt.Errorf(
				"exec: argument %d is a %s value; use a scalar path (e.g. $name.field) or the file adapter (file:$name)",
				i+1, aa.Value.Kind())
		default:
			resolved[i] = a
		}
	}

	argv := argTexts(resolved)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	cap := execCapture{Argv: argv}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return execCapture{}, fmt.Errorf("exec %s: %w", argv[0], err)
		}
		cap.ExitCode = exitErr.ExitCode()
	}
	cap.Stdout = stdout.String()
	cap.Stderr = stderr.String()
	return cap, nil
}

// replExec runs an external command for the [cmd] callsite.
//
// In strict mode (the default), exit 0 returns a structured result
// and non-zero exit returns an error. This keeps the common case
// clean for require ok exec and assert ok exec.
//
// In status mode (exec status ...), non-zero exit is not an error.
// The structured result is returned for all exit codes, with
// exit_code reflecting the actual status. Only genuine launch
// failures (command not found, permission denied) produce errors.
// Status mode is for commands like diff, grep -q, and cmp where
// non-zero exit is a domain result rather than an execution failure.
func replExec(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("exec requires at least one argument")
	}

	statusMode := false
	if argText(args[0]) == "status" {
		statusMode = true
		args = args[1:]
		if len(args) == 0 {
			return shell.Value{}, fmt.Errorf("exec status requires at least one argument")
		}
	}

	cap, err := runExternal(ctx, args)
	if err != nil {
		return shell.Value{}, err
	}
	if cap.ExitCode != 0 && !statusMode {
		msg := fmt.Sprintf("exec %s: exit status %d", strings.Join(cap.Argv, " "), cap.ExitCode)
		if cap.Stderr != "" {
			msg += ": " + strings.TrimRight(cap.Stderr, "\n")
		}
		return shell.Value{}, errors.New(msg)
	}

	result := execResult{
		Argv:     cap.Argv,
		Stdout:   cap.Stdout,
		Stderr:   cap.Stderr,
		ExitCode: cap.ExitCode,
	}
	val, err := shell.ValueFromStruct(result)
	if err != nil {
		return shell.Value{}, fmt.Errorf("exec: build result: %w", err)
	}

	if cap.Stdout != "" {
		if err := cli.PrintOut(cap.Stdout); err != nil {
			return shell.Value{}, err
		}
	}

	return val.WithKind(shell.OriginExecResult), nil
}
