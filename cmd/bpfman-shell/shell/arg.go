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
	case NilArg:
		return v.Span
	case MissingArg:
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

// NilArg is the null value at an arg boundary. Produced by
// valueToArg when a variable expression resolves to a nil Value:
// `$got.status.links` where the JSON value is `null`,
// `$prog.status.stats` where stats is `null`, and similar shape-
// test inputs. Command handlers that meaningfully accept null
// (jq, print, the strict-nil / present predicates) inspect this
// variant; other handlers can either reject NilArg explicitly or
// fall through to their default unsupported-type diagnostic.
//
// Span is the originating $name reference's source extent so a
// downstream "this command can't take null" diagnostic frames at
// the right token.
type NilArg struct {
	Span
}

// MissingArg is the "field absent from the value tree" outcome at
// an arg boundary. Produced when a variable expression's path
// names a field that does not exist (typically because an
// omitempty-elided producer chose not to emit it). Distinct from
// NilArg, which is the explicit-null outcome.
//
// The two variants exist so the shape-test predicates can
// distinguish "the field is missing from the shape" (a contract
// regression) from "the field is present and null" (the producer's
// way of saying the concept does not apply). Command handlers
// that do not meaningfully accept missing fields surface their
// own diagnostic when they encounter MissingArg.
type MissingArg struct {
	Name string // bare variable name without "$"
	Path string // dotted/indexed path expression after the name
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
// argument-expansion boundary: Predicate entries (`not-empty`,
// `nil`, `empty`) leave Value zero and let the consumer decide
// the assertion semantics; SubBlock entries carry a fully-
// expanded nested matches block to evaluate against the sub-value
// at the entry's Path; value-pattern entries store the resolved
// value of the pattern expression. Exhaustive reflects the
// `matches exhaustive` keyword form -- the host command (today
// only the assert verb dispatcher) uses it to enable the
// structural-coverage check. Span covers the whole
// `matches { ... }` expression.
type MatchesBlockArg struct {
	Entries    []MatchesBlockEntry
	Exhaustive bool
	Span
}

// MatchesBlockEntry is the post-expansion form of one matches row.
// Path is verbatim from the source. Exactly one of Predicate /
// SubBlock / Value is meaningful: Predicate non-empty names a
// bareword predicate (`not-empty`, `nil`, `empty`); SubBlock
// non-nil means the pattern is a nested matches block; otherwise
// Value carries the resolved pattern expression's value for
// equality comparison.
type MatchesBlockEntry struct {
	Path      string
	Value     Value
	SubBlock  *MatchesBlockArg
	Predicate string
	Span
}

func (WordArg) isArg()            {}
func (NilArg) isArg()             {}
func (MissingArg) isArg()         {}
func (QuotedArg) isArg()          {}
func (ScalarValueArg) isArg()     {}
func (StructuredValueArg) isArg() {}
func (AdapterArg) isArg()         {}
func (MatchesBlockArg) isArg()    {}
