package shell

// Arg is the post-expansion representation of a command argument. It
// is a sealed sum type that preserves distinctions lost in []string:
// whether an argument was literal command syntax, a quoted literal,
// an eagerly resolved scalar value, or a structured shell value
// passed directly to a command.
type Arg interface {
	isArg()
}

// WordArg is literal command text supplied by the user: command
// names, flags, paths, numeric IDs. It was never a variable
// reference.
type WordArg struct {
	Text string
}

// QuotedArg preserves user quoting as a distinct syntactic form.
// A quoted path with spaces is distinct from an unquoted flag.
type QuotedArg struct {
	Text string
}

// ScalarValueArg is a value produced by variable expansion. The
// original variable reference has been resolved to a string. It is
// semantically distinct from WordArg because it came from a
// variable, not from user-typed literal text.
type ScalarValueArg struct {
	Text string
}

// StructuredValueArg is a resolved structured variable value passed
// directly to a command. The command parser decides how to use it
// (e.g. extract .record.program_id). Name holds the variable name
// without the $ prefix.
type StructuredValueArg struct {
	Name  string
	Value Value
}

// AdapterArg is a resolved adapter invocation from inline adapter
// syntax (e.g. file:$var.path in exec argument position). Adapter
// is the adapter name, Value is the resolved REPL value (scalar or
// structured), and Name/Path are retained for display.
type AdapterArg struct {
	Adapter string
	Name    string
	Path    string
	Value   Value
}

// MatchesBlockArg carries the entries of a parsed `matches { ... }`
// block to the host command. Patterns are evaluated eagerly at the
// argument-expansion boundary: NotEmpty entries set NotEmpty true
// and leave Value zero; value-pattern entries store the resolved
// scalar (or structured value, kept for completeness) of the
// pattern expression.
type MatchesBlockArg struct {
	Entries []MatchesBlockEntry
}

// MatchesBlockEntry is the post-expansion form of one matches row.
// Path is verbatim from the source. Exactly one of NotEmpty / Value
// is meaningful.
type MatchesBlockEntry struct {
	Path     string
	Value    Value
	NotEmpty bool
	Loc      Loc
}

func (WordArg) isArg()            {}
func (QuotedArg) isArg()          {}
func (ScalarValueArg) isArg()     {}
func (StructuredValueArg) isArg() {}
func (AdapterArg) isArg()         {}
func (MatchesBlockArg) isArg()    {}
