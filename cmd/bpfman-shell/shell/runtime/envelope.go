package runtime

import (
	"encoding/json"
	"strconv"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
)

// Envelope is the result envelope returned alongside every command
// form. It carries execution metadata only:
//
//	ok      true iff the underlying command exited zero. For an
//	        async job that was killed, ok stays false because
//	        the process did not exit zero; the script
//	        distinguishes "expected termination" from "real
//	        failure" via the killed and signal fields.
//	code    exit code (subprocess) or 0/1 (in-process). For a
//	        signalled process, code is the conventional
//	        128+signum (matching shell convention), so
//	        SIGTERM yields 143, SIGUSR1 yields 138, and so on.
//	        A trap that exits with its own status overrides
//	        the convention.
//	stdout  captured stdout, or in-process renderable
//	stderr  captured stderr, or in-process error message
//	killed  true when the script called 'kill $job' against
//	        this job. Lets the script say "the process exited
//	        non-zero, but I asked for it" without conflating
//	        that case with a real failure.
//	signal  short signal name (TERM, KILL, USR1, ...) when the
//	        process exited via signal, empty otherwise.
//	pid     process id, present only when HasPID is true; the
//	        pid field is omitted from the wrapped Value's
//	        path-walkable shape when HasPID is false.
//
// The provider's typed payload (the primary) lives in its own
// slot, not on the envelope. See BindResult.
type Envelope struct {
	OK     bool
	Code   int
	Stdout string
	Stderr string
	Killed bool
	Signal string
	HasPID bool
	PID    int
}

// ValueFromEnvelope wraps e as a Value. The Value carries e in the
// origin slot (recoverable via Origin()) and a JSON-tree mirror in
// the standard v slot so the path machinery resolves $r.ok,
// $r.code, $r.stdout, $r.stderr, and $r.pid (when HasPID).
func ValueFromEnvelope(e Envelope) Value {
	mirror := map[string]any{
		"ok":     e.OK,
		"code":   numFromInt(e.Code),
		"stdout": e.Stdout,
		"stderr": e.Stderr,
		"killed": e.Killed,
		"signal": e.Signal,
	}
	if e.HasPID {
		mirror["pid"] = numFromInt(e.PID)
	}
	return Value{v: mirror, origin: e, kind: semantics.OriginEnvelope}
}

// OkEnvelope returns the canonical "command succeeded with no
// specific payload" envelope: OK=true, Code=0, no streams.
// Used by dispatch sites that synthesize a successful outcome
// from scratch -- a def body that ran cleanly, a poll
// attempt that satisfied its body without retry. Sites that
// have real outcome data (a subprocess's captured streams,
// an actual exit code from RunExternal) build Envelope{...}
// directly because they have richer information than this
// helper can express.
func OkEnvelope() Envelope {
	return Envelope{OK: true}
}

// FailEnvelope returns the canonical "command failed without a
// more specific source" envelope: OK=false, Code=1, no
// streams. The Code field tracks OK so an envelope is never
// internally inconsistent (OK=false / Code=0 reads as
// "succeeded but not ok" and confuses every renderer that
// prints both fields). Sites that have a specific failure
// code (a subprocess that exited non-zero, a guard-failure
// envelope from a registered handler) build Envelope{...}
// directly with the real code.
func FailEnvelope() Envelope {
	return Envelope{OK: false, Code: 1}
}

// FailEnvelopeFromError returns a FailEnvelope with err's
// message in Stderr. Used by structural-failure paths where
// the failure has no captured stderr of its own (a defer
// whose dispatch failed without ever launching, a
// builtin-resolution error before the handler ran).
func FailEnvelopeFromError(err error) Envelope {
	e := FailEnvelope()
	if err != nil {
		e.Stderr = err.Error()
	}
	return e
}

// BindResult is what an ExecBind hook returns. Rc is the result
// envelope; Primary is the provider's primary result. For
// providers that produce a typed payload, Primary is the typed
// Value. For providers that produce no separate payload (exec,
// bpftool, wait), Primary is ValueFromEnvelope(Rc) so a
// single-name bind hands the script a uniformly-shaped value to
// inspect. On failure for typed-payload providers, Primary is the
// zero Value.
type BindResult struct {
	Rc      Envelope
	Primary Value
}

// numFromInt wraps a Go int as a json.Number so the path-walker and
// Scalar() resolve it through the same code path that handles
// numbers parsed from JSON via UseNumber.
func numFromInt(n int) json.Number {
	return json.Number(strconv.Itoa(n))
}
