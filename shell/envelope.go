package shell

import (
	"encoding/json"
	"strconv"
)

// Envelope is the captured-result shape returned by every command
// form. The fields are:
//
//	ok      whether the command completed successfully
//	code    exit code (subprocess) or 0/1 (in-process)
//	stdout  captured stdout, or in-process renderable
//	stderr  captured stderr, or in-process error message
//	value   typed payload from registered providers; zero Value
//	        when the command produced none (external commands
//	        and the explicit exec escape hatch leave Value zero)
//	pid     process id, present only when HasPID is true; the
//	        pid field is omitted from the wrapped Value's
//	        path-walkable shape when HasPID is false
//
// The struct is the boundary representation between commands and
// the language. Producers populate it directly; consumers either
// recover it via Value.Origin() to keep typed access to Value, or
// use $name.ok / $name.code / $name.value... where the path
// walker resolves through the JSON-tree mirror.
type Envelope struct {
	OK     bool
	Code   int
	Stdout string
	Stderr string
	Value  Value
	HasPID bool
	PID    int
}

// ValueFromEnvelope wraps e as a Value. The Value carries e in the
// origin slot (recoverable via Origin()) and a JSON-tree mirror in
// the standard v slot so the path machinery resolves $r.ok,
// $r.code, $r.stdout, $r.stderr, $r.value..., and $r.pid (when
// HasPID). The inner e.Value's JSON tree is exposed under "value";
// its own typed origin is preserved through Origin(), not through
// the path lookup.
func ValueFromEnvelope(e Envelope) Value {
	mirror := map[string]any{
		"ok":     e.OK,
		"code":   numFromInt(e.Code),
		"stdout": e.Stdout,
		"stderr": e.Stderr,
		"value":  e.Value.Raw(),
	}
	if e.HasPID {
		mirror["pid"] = numFromInt(e.PID)
	}
	return Value{v: mirror, origin: e, kind: OriginEnvelope}
}

// numFromInt wraps a Go int as a json.Number so the path-walker and
// Scalar() resolve it through the same code path that handles
// numbers parsed from JSON via UseNumber.
func numFromInt(n int) json.Number {
	return json.Number(strconv.Itoa(n))
}
