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
			want: `$src matches exhaustive { status.id: $want, status.meta: matches { name: "demo" } }`,
		},
	}

	for _, tc := range tests {
		if got := FormatExprSource(tc.expr); got != tc.want {
			t.Fatalf("%s: FormatExprSource() = %q, want %q", tc.name, got, tc.want)
		}
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
