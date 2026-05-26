package syntax

import "testing"

func TestFormatExprSource_ComplexForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		expr Expr
		want string
	}{
		{
			name: "interpolation",
			expr: &InterpStringExpr{
				Segments: []InterpStringSegment{
					{Literal: "hello-"},
					{Expr: &VarRefExpr{Name: "name"}},
					{Literal: "-"},
					{Expr: &BinaryExpr{
						Left:  &VarRefExpr{Name: "lhs"},
						Op:    "+",
						Right: &VarRefExpr{Name: "rhs"},
					}},
				},
			},
			want: `"hello-${name}-${$lhs + $rhs}"`,
		},
		{
			name: "thread",
			expr: &ThreadExpr{
				LHS: &VarRefExpr{Name: "src"},
				Args: []Expr{
					&LiteralExpr{Text: "jq"},
					&LiteralExpr{Text: ".id", Quoted: true},
				},
			},
			want: `$src |> jq ".id"`,
		},
		{
			name: "matches",
			expr: &MatchesExpr{
				Target: &VarRefExpr{Name: "src"},
				Block: &MatchesBlockExpr{
					Exhaustive: true,
					Entries: []MatchEntry{
						{
							Path:    "status.id",
							Pattern: &VarRefExpr{Name: "want"},
						},
						{
							Path: "status.meta",
							SubBlock: &MatchesBlockExpr{
								Entries: []MatchEntry{{
									Path:    "name",
									Pattern: &LiteralExpr{Text: "demo", Quoted: true},
								}},
							},
						},
					},
				},
			},
			want: "$src matches exhaustive { status.id: $want\n status.meta: matches { name: \"demo\" } }",
		},
	}

	for _, tc := range tests {
		if got := FormatExprSource(tc.expr); got != tc.want {
			t.Fatalf("%s: FormatExprSource() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFormatExprSource_MatchesRoundTripsThroughParser(t *testing.T) {
	t.Parallel()

	// Rendering a matches block from the AST and feeding it
	// back through the parser must succeed: the formatter
	// previously emitted entries separated by commas, but the
	// parser explicitly rejects commas inside a matches block
	// ("matches: ',' is not a valid entry separator; entries
	// are separated by newlines"). The format-then-reparse
	// loop is the load-bearing contract here -- diagnostics
	// and developer tooling that round-trip the printed form
	// rely on it being a re-parseable string.
	expr := &MatchesExpr{
		Target: &VarRefExpr{Name: "src"},
		Block: &MatchesBlockExpr{
			Entries: []MatchEntry{
				{Path: "id", Pattern: &VarRefExpr{Name: "want"}},
				{Path: "name", Pattern: &LiteralExpr{Text: "demo", Quoted: true}},
			},
		},
	}
	src := "let r = " + FormatExprSource(expr)
	tokens, err := Tokenise(src)
	if err != nil {
		t.Fatalf("tokenise reformatted source %q: %v", src, err)
	}
	if _, err := Parse(tokens); err != nil {
		t.Fatalf("reparse reformatted source %q: %v", src, err)
	}
}

func TestFormatAssertClauseSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		clause AssertClause
		want   string
	}{
		{
			name: "expr",
			clause: &AssertExprClause{
				Expr: &BinaryExpr{
					Left:  &VarRefExpr{Name: "lhs"},
					Op:    "==",
					Right: &LiteralExpr{Text: "42"},
				},
			},
			want: `$lhs == 42`,
		},
		{
			name: "command",
			clause: &AssertCommandClause{
				Negate: true,
				Head:   "ok",
				Args: []Expr{
					&LiteralExpr{Text: "exec"},
					&LiteralExpr{Text: "false"},
				},
			},
			want: `not ok exec false`,
		},
	}

	for _, tc := range tests {
		if got := FormatAssertClauseSource(tc.clause); got != tc.want {
			t.Fatalf("%s: FormatAssertClauseSource() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
