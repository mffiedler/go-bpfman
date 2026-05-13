// SourceLoc is the file/line/col triple the REPL threads through
// every diagnostic. Lives in the repl package because the loop,
// the dispatcher, the renderer, and every handler that emits a
// failure path share it; previous incarnation was an unexported
// struct in cmd/bpfman-shell/check.go.

package repl

import "fmt"

// SourceLoc identifies a position in a script file. The zero
// value means "no location" and formats as the empty string, so
// interactive and piped-stdin modes are unaffected.
type SourceLoc struct {
	File string
	Line int
	Col  int
}

// String renders the location as `file:line: ` (or
// `file:line:col: `), suitable as a prefix for error messages.
// Returns the empty string for the zero value.
func (l SourceLoc) String() string {
	if l.File == "" {
		return ""
	}
	if l.Col > 0 {
		return fmt.Sprintf("%s:%d:%d: ", l.File, l.Line, l.Col)
	}
	return fmt.Sprintf("%s:%d: ", l.File, l.Line)
}

// Cite returns the bare `file:line[:col]` citation without the
// trailing `: ` separator that String adds for inline error
// prefixes. Used when the location is rendered as a value in
// its own right (Job.Origin, for example, so the scope-exit
// leak diagnostic can show where the start lived).
func (l SourceLoc) Cite() string {
	if l.File == "" {
		return ""
	}
	if l.Col > 0 {
		return fmt.Sprintf("%s:%d:%d", l.File, l.Line, l.Col)
	}
	return fmt.Sprintf("%s:%d", l.File, l.Line)
}
