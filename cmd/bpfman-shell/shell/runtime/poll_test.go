package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"
)

func TestPoll_RetriesUntilReady(t *testing.T) {
	t.Parallel()

	src := `
poll timeout 100ms every 1ms {
  let state <- probe
  retry "waiting for ready" unless $state == ready
}
print after
`
	prog := parseProgram(t, src)
	var calls []execCall
	attempts := 0
	env := &Env{
		Session: NewSession(),
		ExecCommand: func(args []Arg, span source.Span) (Value, error) {
			calls = append(calls, execCall{Lane: "command", Argv: renderArgv(args)})
			return Value{}, nil
		},
		ExecBind: func(args []Arg, span source.Span) (BindResult, error) {
			calls = append(calls, execCall{Lane: "bind", Argv: renderArgv(args)})
			attempts++
			if attempts < 3 {
				return BindResult{Rc: OkEnvelope(), Primary: StringValue("pending")}, nil
			}
			return BindResult{Rc: OkEnvelope(), Primary: StringValue("ready")}, nil
		},
	}
	lp, err := lowerToIR(prog)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if err := Exec(lp, env); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	assertCalls(t, calls, []execCall{
		{Lane: "bind", Argv: "probe"},
		{Lane: "bind", Argv: "probe"},
		{Lane: "bind", Argv: "probe"},
		{Lane: "command", Argv: "print after"},
	})
}

func TestPoll_RequireIsFatal(t *testing.T) {
	t.Parallel()

	src := `
def helper() { require false }
poll timeout 20ms every 1ms {
  helper
}
print after
`
	err := runScriptError(t, src, nil)
	if err == nil {
		t.Fatal("expected require failure")
	}
	if !strings.Contains(err.Error(), "require failed: false") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPoll_HelperRetryFailedDeferIsFatal(t *testing.T) {
	t.Parallel()

	// A helper def invoked from a poll body that itself runs
	// `retry` must be treated as part of the enclosing attempt:
	// if a defer registered in the helper fails during the
	// retry-time unwind, the attempt cannot make forward
	// progress, so the poll must halt rather than spin to the
	// timeout while the same cleanup keeps failing each round.
	//
	// The retry sequence in a helper emits RunDefersAttemptFatal,
	// the same policy the lexical poll body uses, but the
	// helper runs on its own executor where ex.polls is empty.
	// Without a fix the executor's "fatal cleanup" branch (gated
	// on len(ex.polls) > 0) is skipped, the failure is dropped,
	// the helper signals retry to the caller, and the poll
	// burns its budget. The discriminator is the error string:
	// the bug returns "poll timed out"; the fix returns the
	// attempt-local defer diagnostic immediately.
	src := `
def helper() {
  defer cleanup
  retry "always"
}
poll timeout 50ms every 1ms {
  helper
}
`
	r := &recorder{
		rc: func(args []Arg) Envelope {
			if argText0(recordedCall{args: args}) == "cleanup" {
				return Envelope{OK: false, Code: 2, Stderr: "broken"}
			}
			return Envelope{OK: true}
		},
	}
	env := &Env{
		Session:  NewSession(),
		ExecBind: r.execBind,
		ExecCommand: func(args []Arg, _ source.Span) (Value, error) {
			return Value{}, nil
		},
	}

	err := runProgramWithEnv(t, src, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "defer failed",
		"helper defer failure must surface as a fatal cleanup diagnostic, got %q", err.Error())
	assert.NotContains(t, err.Error(), "poll timed out",
		"helper defer failure must halt the poll immediately, not let it spin to the timeout")
}

func TestPoll_TimeoutUsesRetryMessage(t *testing.T) {
	t.Parallel()

	src := `
poll timeout 5ms every 1ms {
  retry "waiting for ack"
}
`
	err := runScriptError(t, src, nil)
	if err == nil {
		t.Fatal("expected poll timeout")
	}
	if !strings.Contains(err.Error(), "poll timed out") {
		t.Fatalf("unexpected timeout error: %v", err)
	}
	if !strings.Contains(err.Error(), "waiting for ack") {
		t.Fatalf("timeout lost last retry message: %v", err)
	}
}
