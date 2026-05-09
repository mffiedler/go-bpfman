package shell

import (
	"encoding/json"
	"strconv"
)

// Envelope is the result envelope returned alongside every command
// form. It carries execution metadata only:
//
//	ok      whether the command completed successfully
//	code    exit code (subprocess) or 0/1 (in-process)
//	stdout  captured stdout, or in-process renderable
//	stderr  captured stderr, or in-process error message
//	pid     process id, present only when HasPID is true; the
//	        pid field is omitted from the wrapped Value's
//	        path-walkable shape when HasPID is false
//
// The provider's typed payload (the primary) lives in its own
// slot, not on the envelope. See BindResult.
type Envelope struct {
	OK     bool
	Code   int
	Stdout string
	Stderr string
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
	}
	if e.HasPID {
		mirror["pid"] = numFromInt(e.PID)
	}
	return Value{v: mirror, origin: e, kind: OriginEnvelope}
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
