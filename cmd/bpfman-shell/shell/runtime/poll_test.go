package runtime

import (
	"strings"
	"testing"

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
