package runtime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
)

// OriginMismatchError is returned when a command parser receives a
// Value whose origin kind does not match any of the accepted kinds.
// Consumers match on this type to produce command-specific error
// messages.
type OriginMismatchError struct {
	VarName string
	Got     semantics.OriginKind
	Want    []semantics.OriginKind
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
func ExpectOrigin(v Value, varName string, wanted ...semantics.OriginKind) error {
	got := v.Kind()
	if got == semantics.OriginUnknown {
		return nil
	}
	if slices.Contains(wanted, got) {
		return nil
	}
	return &OriginMismatchError{VarName: varName, Got: got, Want: wanted}
}
