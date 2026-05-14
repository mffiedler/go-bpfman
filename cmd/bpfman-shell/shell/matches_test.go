package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseMatchesCmd parses src and expects the single statement to be
// a CommandStmt whose final arg is a *MatchesBlockExpr. It returns
// the command's full Args list and the block.
func parseMatchesCmd(t *testing.T, src string) (*CommandStmt, *MatchesBlockExpr) {
	t.Helper()
	prog, err := parseSource(t, src)
	require.NoError(t, err)
	cmd, ok := firstStmt(t, prog).(*CommandStmt)
	require.True(t, ok, "want CommandStmt, got %T", firstStmt(t, prog))
	require.NotEmpty(t, cmd.Args)
	block, ok := cmd.Args[len(cmd.Args)-1].(*MatchesBlockExpr)
	require.True(t, ok, "last arg should be *MatchesBlockExpr, got %T", cmd.Args[len(cmd.Args)-1])
	return cmd, block
}

func TestParse_MatchesBlock_SingleEntry(t *testing.T) {
	t.Parallel()

	cmd, block := parseMatchesCmd(t, `assert $prog matches { record.meta.name: foo }`)

	// The "matches" keyword is consumed by the block syntax.
	require.Len(t, cmd.Args, 3)
	assert.Equal(t, "assert", cmd.Args[0].(*LiteralExpr).Text)
	assert.Equal(t, "prog", cmd.Args[1].(*VarRefExpr).Name)

	require.Len(t, block.Entries, 1)
	assert.Equal(t, "record.meta.name", block.Entries[0].Path)
	assert.Empty(t, block.Entries[0].Predicate)
	lit, ok := block.Entries[0].Pattern.(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "foo", lit.Text)
}

func TestParse_MatchesBlock_MultiEntry_NewlineSeparated(t *testing.T) {
	t.Parallel()

	src := `assert $prog matches {
    record.meta.name: foo
    status.kernel.id: $pid
    status.kernel.tag: not-empty
}`
	_, block := parseMatchesCmd(t, src)
	require.Len(t, block.Entries, 3)

	assert.Equal(t, "record.meta.name", block.Entries[0].Path)
	assert.Equal(t, "foo", block.Entries[0].Pattern.(*LiteralExpr).Text)

	assert.Equal(t, "status.kernel.id", block.Entries[1].Path)
	assert.Equal(t, "pid", block.Entries[1].Pattern.(*VarRefExpr).Name)

	assert.Equal(t, "status.kernel.tag", block.Entries[2].Path)
	assert.Equal(t, "not-empty", block.Entries[2].Predicate)
	assert.Nil(t, block.Entries[2].Pattern)
}

// Commas are not a valid entry separator inside a matches block --
// the block is line-oriented (one path-pattern relation per line).
// See the def parameter list for the contrasting case where commas
// are the right tool: parameters are a value list, not a table.
//
// Two diagnostic shapes appear: a stand-alone `,` token (when
// surrounded by whitespace, as on a line of its own) hits the
// "',' is not a valid entry separator" path; a `,` glued to the
// preceding token (the common `1,` shape — the lexer treats `,`
// as a word-interior character because the rest of the language
// has no syntactic use for it) hits the "trailing ',' on ..."
// path.  Both share the "entries are separated by newlines"
// suffix; the test asserts that.
func TestParse_MatchesBlock_RejectsCommaSeparator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{"trailing-glued", `assert $p matches { a.b: 1, c.d: 2 }`},
		{"trailing-glued-final", `assert $p matches { a.b: 1, }`},
		{"trailing-glued-multiline", `assert $p matches { a.b: 1,
                                            c.d: 2 }`},
		{"standalone-on-own-line", `assert $p matches {
    a.b: 1
    ,
    c.d: 2
}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSource(t, tc.src)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "matches:")
			assert.Contains(t, err.Error(), "entries are separated by newlines")
		})
	}
}

// `;` is not a valid entry separator either: a matches block is
// line-oriented (one path-pattern relation per line), not a
// sequence of statements.  The same diagnostic shape as the comma
// rejection points the user at the newline rule.
func TestParse_MatchesBlock_RejectsSemicolonSeparator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{"between-entries", `assert $p matches { a.b: 1; c.d: 2 }`},
		{"trailing", `assert $p matches { a.b: 1; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSource(t, tc.src)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "';' is not a valid entry separator")
		})
	}
}

func TestParse_MatchesBlock_ColonSpacingForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{"glued-trailing", `assert $p matches { a.b: x }`},
		{"standalone", `assert $p matches { a.b : x }`},
		{"glued-leading", `assert $p matches { a.b :x }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, block := parseMatchesCmd(t, tc.src)
			require.Len(t, block.Entries, 1)
			assert.Equal(t, "a.b", block.Entries[0].Path)
			assert.Equal(t, "x", block.Entries[0].Pattern.(*LiteralExpr).Text)
		})
	}
}

func TestParse_MatchesBlock_QuotedPattern(t *testing.T) {
	t.Parallel()

	_, block := parseMatchesCmd(t, `assert $p matches { a.b: "hello world" }`)
	require.Len(t, block.Entries, 1)
	lit := block.Entries[0].Pattern.(*LiteralExpr)
	assert.Equal(t, "hello world", lit.Text)
	assert.True(t, lit.Quoted)
}

// `not-empty' written bare is the unary-predicate pattern: assert
// the field is non-empty. Quoting it escapes back to a literal
// compare against the string "not-empty". This is the
// disambiguation rule for any bare-word that doubles as a keyword
// in pattern position; the same escape works for `true`, `false`,
// and any future predicate.
func TestParse_MatchesBlock_BareNotEmpty_IsPredicate(t *testing.T) {
	t.Parallel()

	_, block := parseMatchesCmd(t, `assert $p matches { a.b: not-empty }`)
	require.Len(t, block.Entries, 1)
	assert.Equal(t, "not-empty", block.Entries[0].Predicate, "bare not-empty must register the unary predicate")
	assert.Nil(t, block.Entries[0].Pattern, "predicate entry has no expression pattern")
}

func TestParse_MatchesBlock_QuotedNotEmpty_IsLiteral(t *testing.T) {
	t.Parallel()

	for _, src := range []string{
		`assert $p matches { a.b: "not-empty" }`,
		`assert $p matches { a.b: 'not-empty' }`,
	} {
		_, block := parseMatchesCmd(t, src)
		require.Len(t, block.Entries, 1)
		assert.Empty(t, block.Entries[0].Predicate, "quoted form must NOT trigger the predicate path: %s", src)
		lit, ok := block.Entries[0].Pattern.(*LiteralExpr)
		require.True(t, ok, "quoted form must produce a literal expression: %s", src)
		assert.Equal(t, "not-empty", lit.Text)
		assert.True(t, lit.Quoted, "the literal must remember it was quoted: %s", src)
	}
}

func TestParse_MatchesBlock_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{"missing colon", `assert $p matches { a.b foo }`},
		{"missing pattern", `assert $p matches { a.b: }`},
		{"empty path", `assert $p matches { :foo }`},
		{"unterminated", `assert $p matches { a.b: foo`},
		{"matches at start of line", `matches { a.b: foo }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSource(t, tc.src)
			require.Error(t, err)
		})
	}
}

func TestParse_MatchesBlock_RequiresMatchesKeyword(t *testing.T) {
	t.Parallel()

	// The block syntax is gated on the bare keyword "matches"
	// preceding `{`. With the keyword the assert grows a
	// MatchesBlockExpr arg; without it the prior parser behaviour
	// is retained and the host command's args do not contain a
	// MatchesBlockExpr.
	prog, err := parseSource(t, `assert $p matches { a.b: foo }`)
	require.NoError(t, err)
	cmd, ok := firstStmt(t, prog).(*CommandStmt)
	require.True(t, ok)
	_, isMB := cmd.Args[len(cmd.Args)-1].(*MatchesBlockExpr)
	assert.True(t, isMB)
}
