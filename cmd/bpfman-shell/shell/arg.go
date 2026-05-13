package shell

// Arg is the post-expansion representation of a command argument. It
// is a sealed sum type that preserves distinctions lost in []string:
// whether an argument was literal command syntax, a quoted literal,
// an eagerly resolved scalar value, or a structured shell value
// passed directly to a command. Every Arg variant embeds Span so
// command-handler parsers can frame argument-position errors at
// the source token (or, for variable-resolved args, at the
// originating $name reference). ArgSpan extracts the Span via a
// type switch since Go interfaces cannot share embedded fields.
type Arg interface {
	isArg()
}

// ArgSpan returns the source extent of a, or a zero Span when a is
// nil or a variant the helper does not recognise. Use from
// command-handler parsers to attach a Span to an argument-position
// diagnostic (missing flag, unknown view, unparseable ID).
func ArgSpan(a Arg) Span {
	switch v := a.(type) {
	case WordArg:
		return v.Span
	case QuotedArg:
		return v.Span
	case ScalarValueArg:
		return v.Span
	case StructuredValueArg:
		return v.Span
	case AdapterArg:
		return v.Span
	case MatchesBlockArg:
		return v.Span
	}
	return Span{}
}

// WordArg is literal command text supplied by the user: command
// names, flags, paths, numeric IDs. It was never a variable
// reference.
type WordArg struct {
	Text string
	Span
}

// QuotedArg preserves user quoting as a distinct syntactic form.
// A quoted path with spaces is distinct from an unquoted flag.
type QuotedArg struct {
	Text string
	Span
}

// ScalarValueArg is a value produced by variable expansion. The
// original variable reference has been resolved to a string. It is
// semantically distinct from WordArg because it came from a
// variable, not from user-typed literal text. Span is the
// originating $name reference's source extent.
type ScalarValueArg struct {
	Text string
	Span
}

// StructuredValueArg is a resolved structured variable value passed
// directly to a command. The command parser decides how to use it
// (e.g. extract .record.program_id). Name holds the variable name
// without the $ prefix. Span is the originating $name reference.
type StructuredValueArg struct {
	Name  string
	Value Value
	Span
}

// AdapterArg is a resolved adapter invocation from inline adapter
// syntax (e.g. file:$var.path in exec argument position). Adapter
// is the adapter name, Value is the resolved REPL value (scalar or
// structured), and Name/Path are retained for display. Span covers
// the adapter:$var.path expression.
type AdapterArg struct {
	Adapter string
	Name    string
	Path    string
	Value   Value
	Span
}

// MatchesBlockArg carries the entries of a parsed `matches { ... }`
// block to the host command. Patterns are evaluated eagerly at the
// argument-expansion boundary: NotEmpty entries set NotEmpty true
// and leave Value zero; value-pattern entries store the resolved
// scalar (or structured value, kept for completeness) of the
// pattern expression. Span covers the whole `matches { ... }`
// expression.
type MatchesBlockArg struct {
	Entries []MatchesBlockEntry
	Span
}

// MatchesBlockEntry is the post-expansion form of one matches row.
// Path is verbatim from the source. Exactly one of NotEmpty / Value
// is meaningful.
type MatchesBlockEntry struct {
	Path     string
	Value    Value
	NotEmpty bool
	Span
}

func (WordArg) isArg()            {}
func (QuotedArg) isArg()          {}
func (ScalarValueArg) isArg()     {}
func (StructuredValueArg) isArg() {}
func (AdapterArg) isArg()         {}
func (MatchesBlockArg) isArg()    {}
