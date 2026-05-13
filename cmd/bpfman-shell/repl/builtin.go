// Builtin registry: the framework half of the bpfman-shell
// dispatcher. The repl package owns the types, the registry, and
// the lookup; each builtin handler lives in the cmd/bpfman-shell
// package and uses these types to declare itself. The keyword
// registry sits next to the builtin registry because the help
// renderer treats both as documentation sources.

package repl

import (
	"context"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
)

// Ctx is the dispatch context handed to every builtin handler.
// Fat-context by design: the dependency superset across the
// dispatched builtins is small (Ctx, CLI, Mgr, Env, Args, Pos,
// Span) and uniform plumbing keeps each handler's wrapper to a
// few lines. Session is reachable via Env.Session; handler-
// internal toggles (isRequire, origin-style) need no dedicated
// field.
type Ctx struct {
	// Ctx is the cancellation/deadline context for the call.
	// Plumbed from the dispatcher's caller; long-running
	// builtins (start, wait, kill's escalation wait, exec)
	// observe it.
	Ctx context.Context

	// CLI carries the stdout/stderr writers and the
	// PrintOut/PrintErrf helpers.
	CLI *bpfmancli.CLI

	// Mgr is the bpfman manager. Only the assertion verbs and
	// source touch it; most builtins leave it nil-safe.
	Mgr *manager.Manager

	// Env is the active shell environment. Env.Session is the
	// canonical handle to variable bindings, defs, and aliases;
	// jobs and source read Env directly to register background
	// processes and to inherit the caller's scope.
	Env *shell.Env

	// Cmd is the command name as the user typed it (args[0]
	// before slicing).
	Cmd string

	// Args is the argument list with the command name already
	// stripped.
	Args []shell.Arg

	// Pos is the source location of the chunk this builtin was
	// dispatched from.
	Pos SourceLoc

	// Span is the source extent of the originating CommandStmt
	// or BindStmt.
	Span shell.Span
}

// ArgCompleter is a per-builtin completion callback for tokens
// after the command name. tokens[0] is the command itself;
// tokens[1:] are the user-typed arguments. trailingSpace says
// whether the cursor is positioned just after a space (so a new
// token is starting) or in the middle of a partial token (so the
// last entry of tokens is being typed). baseDir is the working
// directory for filesystem completions; the empty string in
// production (relative to the process cwd) and a fixture path in
// tests.
//
// Returning candidates with replace == 0 means "the candidates
// are independent of the partial token". Returning
// replace == len(prefix) signals "rewrite the last len(prefix)
// characters with the chosen candidate".
type ArgCompleter func(session *shell.Session, baseDir string, tokens []string, trailingSpace bool) (candidates []string, replace int)

// Category constants group builtins in the help overview.
const (
	CategorySession = "session" // bindings, defs, aliases
	CategoryIO      = "io"      // external commands, file, jq, print, source
	CategoryAssert  = "assert"  // assert / require
	CategoryJobs    = "jobs"    // start / wait / kill / jobs
	CategoryMeta    = "meta"    // help / version
)

// CategoryLabels maps a category constant to the display label
// used in the help overview. CategoryOrder fixes the rendering
// sequence so the overview reads the same on every run; map
// iteration order would otherwise produce arbitrary output.
var CategoryLabels = map[string]string{
	CategorySession: "Session and bindings",
	CategoryIO:      "I/O and external commands",
	CategoryJobs:    "Async jobs",
	CategoryAssert:  "Assertions",
	CategoryMeta:    "Meta",
}

// CategoryOrder fixes the help-overview render order.
var CategoryOrder = []string{
	CategorySession,
	CategoryIO,
	CategoryJobs,
	CategoryAssert,
	CategoryMeta,
}

// Builtin describes one entry in the registry: how to run it,
// how to complete its arguments, what shape its bind-RHS primary
// produces at static-check time, and how to document itself. The
// doc fields drive 'help' (overview) and 'help <name>' (detail).
// Empty fields degrade gracefully -- a builtin with no Detail
// just shows Usage+Summary on detail lookup, and a nil BindShape
// falls through to the default external-subprocess result
// envelope. Pointer-free because the registry is read-only at
// runtime.
type Builtin struct {
	Name     string
	Handler  func(Ctx) (shell.Value, error)
	Complete ArgCompleter // nil = generic fallthrough

	// BindShape lets the static checker resolve `<- NAME ARGS...`
	// to the correct primary Shape. Wrap a fixed Shape with
	// shell.StaticBindShape; nil leaves the checker on the
	// default external-subprocess result envelope.
	BindShape shell.BindShapeFn

	Category string // Category* constant; ungrouped if empty
	Usage    string // one-line syntax
	Summary  string // one-line description
	Detail   string // multi-paragraph long help; optional
}

// Keyword describes a parser-level form (let, guard, defer, def,
// bpfman) that participates in 'help' but is not part of the
// dispatch table. Keywords have no handler and no per-call
// completer because they are recognised by the parser before
// dispatch ever runs.
type Keyword struct {
	Name    string
	Usage   string
	Summary string
	Detail  string
}

// builtinRegistry is the dispatcher's source of truth. Populated
// at init() time by handler files in cmd/bpfman-shell via
// RegisterBuiltin. Read-only after init; no synchronisation
// needed.
var builtinRegistry = map[string]Builtin{}

// keywordRegistry is the documentation source for parser-level
// forms. Populated by RegisterKeyword at init() time.
var keywordRegistry = map[string]Keyword{}

// RegisterBuiltin adds a builtin to the dispatch and help
// registry. Duplicate names panic at startup because silently
// shadowing is the kind of bug that only surfaces months later.
func RegisterBuiltin(b Builtin) {
	if _, dup := builtinRegistry[b.Name]; dup {
		panic("repl: builtin " + b.Name + " registered twice")
	}
	builtinRegistry[b.Name] = b
}

// RegisterKeyword adds a parser-level form to the help registry.
// Same duplicate-name guard as RegisterBuiltin.
func RegisterKeyword(k Keyword) {
	if _, dup := keywordRegistry[k.Name]; dup {
		panic("repl: keyword " + k.Name + " registered twice")
	}
	keywordRegistry[k.Name] = k
}

// LookupBuiltin returns the registered builtin for the given
// name; the second return is false when no builtin matches.
func LookupBuiltin(name string) (Builtin, bool) {
	b, ok := builtinRegistry[name]
	return b, ok
}

// LookupKeyword returns the registered keyword for the given
// name; the second return is false when no keyword matches.
func LookupKeyword(name string) (Keyword, bool) {
	k, ok := keywordRegistry[name]
	return k, ok
}

// Builtins returns the read-only registry of builtins keyed by
// name. Callers must treat the map as immutable; mutate via
// RegisterBuiltin at init() time only.
func Builtins() map[string]Builtin { return builtinRegistry }

// Keywords returns the read-only registry of parser-level forms
// keyed by name. Same immutability contract as Builtins.
func Keywords() map[string]Keyword { return keywordRegistry }
