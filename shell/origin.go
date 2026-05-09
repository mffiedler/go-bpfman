package shell

import (
	"fmt"
	"slices"
	"strings"
)

// OriginKind identifies what kind of thing a Value represents. It is
// a closed set used for command-parser type checks and for uniform
// error messages. The kind is declared at construction time by the
// code that produces the Value; consumers (command parsers, assert,
// if) check it via ExpectOrigin.
//
// OriginUnknown is the default and acts as a wildcard in ExpectOrigin:
// values with no declared kind (e.g. JSON parsed without explicit
// tagging, map literals, path-lookup results) pass all origin checks.
// This preserves the existing fallback behaviour where the consumer
// tries a structural extraction (capability interface, path lookup)
// regardless of origin.
type OriginKind int

const (
	OriginUnknown OriginKind = iota
	OriginScalar
	OriginBool
	OriginProgram
	OriginLink
	OriginDispatcher
	OriginMap
	OriginExecResult
	// OriginEnvelope is the captured-result envelope returned by
	// every command form under the redesign: a structured shape
	// carrying the success bit ("ok"), exit code, captured stdout
	// and stderr, the typed payload from registered providers
	// ("value"), and an optional pid for asynchronous jobs. Field
	// access on an OriginEnvelope value walks the standard path
	// machinery; the original Go envelope struct is recoverable
	// via Origin() so consumers can pull out the typed payload
	// without a JSON round-trip.
	OriginEnvelope
	// OriginNull is a Value that represents JSON null — a value
	// that is present but whose content is null.  Distinct from
	// an absent (zero) Value: an absent Value is a lookup miss or
	// an uninitialised slot, whereas an OriginNull value is what
	// jq returns when a filter selects a missing field, what
	// commands return when they explicitly produce a null result,
	// and what users can get by asking for one.  The
	// distinction matters at substitution and assignment
	// boundaries: a null is assignable and renderable as "null";
	// an absent value trips "produces no assignable value".
	OriginNull
)

// String returns the canonical name used in error messages.
func (k OriginKind) String() string {
	switch k {
	case OriginUnknown:
		return "unknown"
	case OriginScalar:
		return "scalar"
	case OriginBool:
		return "boolean"
	case OriginProgram:
		return "program"
	case OriginLink:
		return "link"
	case OriginDispatcher:
		return "dispatcher"
	case OriginMap:
		return "map"
	case OriginExecResult:
		return "exec.result"
	case OriginEnvelope:
		return "envelope"
	case OriginNull:
		return "null"
	default:
		return fmt.Sprintf("OriginKind(%d)", int(k))
	}
}

// OriginMismatchError is returned when a command parser receives a
// Value whose origin kind does not match any of the accepted kinds.
// Consumers match on this type to produce command-specific error
// messages.
type OriginMismatchError struct {
	VarName string
	Got     OriginKind
	Want    []OriginKind
}

func (e *OriginMismatchError) Error() string {
	var sb strings.Builder
	if e.VarName != "" {
		fmt.Fprintf(&sb, "variable %q is a %s", e.VarName, e.Got)
	} else {
		fmt.Fprintf(&sb, "value is a %s", e.Got)
	}
	switch len(e.Want) {
	case 0:
		// Should not occur; be defensive.
	case 1:
		fmt.Fprintf(&sb, "; expected %s", e.Want[0])
	default:
		sb.WriteString("; expected one of ")
		for i, w := range e.Want {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(w.String())
		}
	}
	return sb.String()
}

// ExpectOrigin reports nil if the Value's origin kind matches any of
// the wanted kinds, or is OriginUnknown (wildcard). Otherwise it
// returns an *OriginMismatchError.
//
// varName is used only for error messages; pass the variable name
// ("$prog") or the empty string for positional values.
func ExpectOrigin(v Value, varName string, wanted ...OriginKind) error {
	got := v.Kind()
	if got == OriginUnknown {
		return nil
	}
	if slices.Contains(wanted, got) {
		return nil
	}
	return &OriginMismatchError{VarName: varName, Got: got, Want: wanted}
}
