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

// resolveExternalArgs walks args, resolving file: adapter
// values to temp files and rejecting structured-value args
// that cannot flatten into argv text. Returned tempFiles are
// the caller's to remove (typically via defer); they outlive
// the resolve call so the spawned process can read them.
// Shared between runExternal (capture path) and
// runExternalInherit (top-level pass-through path).
func resolveExternalArgs(args []shell.Arg) (argv []string, tempFiles []string, err error) {
	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		switch aa := a.(type) {
		case shell.AdapterArg:
			if aa.Adapter != "file" {
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, fmt.Errorf("unknown adapter %q", aa.Adapter)
			}
			path, terr := writeValueToTemp(aa.Value)
			if terr != nil {
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, fmt.Errorf("adapter file: %w", terr)
			}
			tempFiles = append(tempFiles, path)
			resolved[i] = shell.ScalarValueArg{Text: path, Span: aa.Span}
		case shell.StructuredValueArg:
			for _, f := range tempFiles {
				os.Remove(f)
			}
			return nil, nil, fmt.Errorf(
				"exec: argument %d is a %s value; use a scalar path (e.g. $name.field) or the file adapter (file:$name)",
				i+1, aa.Value.Kind())
		default:
			resolved[i] = a
		}
	}
	return argTexts(resolved), tempFiles, nil
}

// runExternal runs an external command and captures its output.
// Inline adapter arguments (e.g. file:$var.path) are resolved to
// temporary files before the command runs and removed
// unconditionally after. Structured-value arguments are rejected
// because they cannot be flattened into argv text. Non-zero exit
// is reported via execCapture.ExitCode, not as an error: callers
// decide whether non-zero is fatal. Use this on the bind path
// ('let r <- ls') where the script needs the captured output;
// for top-level statement position use runExternalInherit so
// TTY-needing programs (vi, less, htop) work.
func runExternal(ctx context.Context, args []shell.Arg) (execCapture, error) {
	if len(args) == 0 {
		return execCapture{}, fmt.Errorf("exec requires at least one argument")
	}
	argv, tempFiles, err := resolveExternalArgs(args)
	if err != nil {
		return execCapture{}, err
	}
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

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

// runExternalInherit runs an external command with stdio
// connected to the parent: stdin from os.Stdin, stdout/stderr
// to the cli's writers, and (when stdin is a TTY) full
// foreground job control so the child owns the terminal for
// the duration of the call. Interactive programs (vi, less,
// htop, ssh, top) get a real TTY, their output streams to the
// user as it happens, and ^C reaches only the child, not the
// shell. When the child exits the shell reclaims the
// terminal's foreground group and the prompt resumes.
//
// The ctx parameter is intentionally not threaded into
// exec.CommandContext for the spawn: a cancellation of the
// shell's root ctx (a ^C the user intended for the foreground
// program, not for the shell) must not SIGKILL the child via
// cmd.Cancel. With job control in place the child receives
// SIGINT directly through the TTY and handles it itself; the
// shell does not even see the signal while the child holds
// the foreground group. ctx stays on the signature so callers
// do not need to know which exec path they are taking.
//
// Off-TTY callers (script mode, stdin pipe, CI) skip the
// foreground-group dance via fgJob's disabled zero value;
// behaviour there matches the no-job-control case.
func runExternalInherit(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg) (argv []string, exitCode int, err error) {
	_ = ctx
	if len(args) == 0 {
		return nil, 0, fmt.Errorf("exec requires at least one argument")
	}
	argv, tempFiles, err := resolveExternalArgs(args)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	fg := newFgJob()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = fg.SysProcAttr()
	cmd.Stdin = os.Stdin
	cmd.Stdout = cli.Out
	cmd.Stderr = cli.Err

	if rerr := cmd.Start(); rerr != nil {
		return argv, 0, fmt.Errorf("exec %s: %w", argv[0], rerr)
	}

	// Hand the terminal to the child. SIGTTOU is masked at
	// process startup (see init in jobctl_signal.go) so this
	// ioctl from a now-background process does not stop us.
	// A failure here means the child runs without owning the
	// foreground group; the user may see ^C affect the shell
	// rather than the child, but the run still completes.
	_ = fg.Grant(cmd.Process.Pid)
	defer func() { _ = fg.Reclaim() }()

	if rerr := cmd.Wait(); rerr != nil {
		var exitErr *exec.ExitError
		if !errors.As(rerr, &exitErr) {
			return argv, 0, fmt.Errorf("exec %s: %w", argv[0], rerr)
		}
		return argv, exitErr.ExitCode(), nil
	}
	return argv, 0, nil
}

// replExec runs an external command at top-level statement
// position with stdio inherited from the parent: stdin from the
// terminal, stdout/stderr streamed live to the user's writers.
// Interactive programs (vi, less, ssh) get a real TTY; long-
// running programs (make, build) stream progress instead of
// buffering it. Non-zero exit becomes a returned error so the
// chunk is reported as failed; launch failures (command not
// found, permission denied) propagate too. Use this for top-
// level position; the bind path uses runExternal to capture
// into a BindResult.
func replExec(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg) (shell.Value, error) {
	argv, exitCode, err := runExternalInherit(ctx, cli, args)
	if err != nil {
		return shell.Value{}, err
	}
	if exitCode != 0 {
		return shell.Value{}, fmt.Errorf("%s: exit status %d", strings.Join(argv, " "), exitCode)
	}
	return shell.Value{}, nil
}
