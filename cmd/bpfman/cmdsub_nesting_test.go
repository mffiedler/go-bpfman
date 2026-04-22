package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/shell"
)

// stubRunner fakes a CmdRunner by mapping the first argument text to
// a pre-computed Value. Missing entries return an error. Used to
// exercise resolveCmdSubs in isolation without wiring in the full
// REPL dispatch pipeline.
func stubRunner(responses map[string]shell.Value, errors map[string]error) shell.CmdRunner {
	return func(innerArgs []shell.Arg) (shell.Value, error) {
		if len(innerArgs) == 0 {
			return shell.Value{}, assertStubErr("stub runner: empty args")
		}
		key := argText(innerArgs[0])
		if err, ok := errors[key]; ok {
			return shell.Value{}, err
		}
		if v, ok := responses[key]; ok {
			return v, nil
		}
		return shell.Value{}, assertStubErr("stub runner: no response for " + key)
	}
}

type stubErr string

func (s stubErr) Error() string       { return string(s) }
func assertStubErr(s string) stubErr  { return stubErr(s) }

func TestResolveCmdSubs_NoNesting(t *testing.T) {
	args := []shell.Arg{
		shell.WordArg{Text: "foo"},
		shell.WordArg{Text: "bar"},
	}
	out, err := resolveCmdSubs(args, stubRunner(nil, nil))
	require.NoError(t, err)
	assert.Equal(t, args, out, "no CmdSubArg present; output should equal input")
}

func TestResolveCmdSubs_FlattensScalar(t *testing.T) {
	runner := stubRunner(map[string]shell.Value{
		"inner": shell.StringValue("hello"),
	}, nil)
	args := []shell.Arg{
		shell.WordArg{Text: "outer"},
		shell.CmdSubArg{InnerArgs: []shell.Arg{shell.WordArg{Text: "inner"}}},
	}
	out, err := resolveCmdSubs(args, runner)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, shell.WordArg{Text: "outer"}, out[0])
	scalar, ok := out[1].(shell.ScalarValueArg)
	require.True(t, ok, "scalar-producing cmdsub should flatten to ScalarValueArg, got %T", out[1])
	assert.Equal(t, "hello", scalar.Text)
}

func TestResolveCmdSubs_PreservesStructured(t *testing.T) {
	structured := shell.ValueFromMap(map[string]any{"id": "42"}).WithKind(shell.OriginProgram)
	runner := stubRunner(map[string]shell.Value{
		"inner": structured,
	}, nil)
	args := []shell.Arg{
		shell.WordArg{Text: "outer"},
		shell.CmdSubArg{InnerArgs: []shell.Arg{shell.WordArg{Text: "inner"}}},
	}
	out, err := resolveCmdSubs(args, runner)
	require.NoError(t, err)
	require.Len(t, out, 2)
	sva, ok := out[1].(shell.StructuredValueArg)
	require.True(t, ok, "structured cmdsub should become StructuredValueArg, got %T", out[1])
	assert.Equal(t, "", sva.Name, "nested cmdsub results are anonymous (no variable name)")
	assert.Equal(t, shell.OriginProgram, sva.Value.Kind())
}

func TestResolveCmdSubs_PropagatesInnerError(t *testing.T) {
	sentinel := errors.New("inner blew up")
	runner := stubRunner(nil, map[string]error{"inner": sentinel})
	args := []shell.Arg{
		shell.CmdSubArg{InnerArgs: []shell.Arg{shell.WordArg{Text: "inner"}}},
	}
	_, err := resolveCmdSubs(args, runner)
	require.Error(t, err)
	assert.Same(t, sentinel, err, "inner runner error should propagate unwrapped")
}

func TestResolveCmdSubs_NilResultIsError(t *testing.T) {
	runner := stubRunner(map[string]shell.Value{
		"inner": shell.Value{},
	}, nil)
	args := []shell.Arg{
		shell.CmdSubArg{InnerArgs: []shell.Arg{shell.WordArg{Text: "inner"}}},
	}
	_, err := resolveCmdSubs(args, runner)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "produced no value")
}

func TestResolveCmdSubs_Nesting(t *testing.T) {
	// The runner's own invocation, via resolveCmdSubs, needs to
	// handle nested CmdSubArgs. Simulate a three-level nest:
	// [outer [middle [inner]]]. Each runner call expects the inner
	// CmdSubArg to have already been resolved to a ScalarValueArg.
	innermost := shell.StringValue("deep")

	var runner shell.CmdRunner
	runner = func(innerArgs []shell.Arg) (shell.Value, error) {
		resolved, err := resolveCmdSubs(innerArgs, runner)
		if err != nil {
			return shell.Value{}, err
		}
		if len(resolved) == 0 {
			return shell.Value{}, assertStubErr("no args")
		}
		first := argText(resolved[0])
		switch first {
		case "inner":
			return innermost, nil
		case "middle":
			// Expect the nested inner to have been flattened.
			if len(resolved) != 2 {
				return shell.Value{}, assertStubErr("middle: expected 2 args")
			}
			s, ok := resolved[1].(shell.ScalarValueArg)
			if !ok || s.Text != "deep" {
				return shell.Value{}, assertStubErr("middle: inner did not flatten")
			}
			return shell.StringValue("middle-saw-" + s.Text), nil
		case "outer":
			if len(resolved) != 2 {
				return shell.Value{}, assertStubErr("outer: expected 2 args")
			}
			s, ok := resolved[1].(shell.ScalarValueArg)
			if !ok {
				return shell.Value{}, assertStubErr("outer: middle did not flatten")
			}
			return shell.StringValue("outer-saw-" + s.Text), nil
		}
		return shell.Value{}, assertStubErr("unknown: " + first)
	}

	outer := []shell.Arg{
		shell.WordArg{Text: "outer"},
		shell.CmdSubArg{InnerArgs: []shell.Arg{
			shell.WordArg{Text: "middle"},
			shell.CmdSubArg{InnerArgs: []shell.Arg{
				shell.WordArg{Text: "inner"},
			}},
		}},
	}
	val, err := runner(outer)
	require.NoError(t, err)
	s, err := val.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "outer-saw-middle-saw-deep", s)
}

func TestDisplayName(t *testing.T) {
	assert.Equal(t, "$prog", displayName("prog"))
	assert.Equal(t, "<command result>", displayName(""))
}
