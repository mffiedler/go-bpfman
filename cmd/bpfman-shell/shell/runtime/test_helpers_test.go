package runtime

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/check"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/ir"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/lower"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
)

func lowerToIR(prog *syntax.Program) (*ir.Program, error) {
	return lower.Lower(prog)
}

func execParsedProgram(t *testing.T, prog *syntax.Program, env *Env) error {
	t.Helper()
	lp, err := lowerToIR(prog)
	if err != nil {
		return err
	}
	return Exec(lp, env)
}

func execSourceProgram(t *testing.T, src string, env *Env) error {
	t.Helper()
	prog, err := parseSource(t, src)
	require.NoError(t, err)
	return execParsedProgram(t, prog, env)
}

func parseSource(t *testing.T, src string) (*syntax.Program, error) {
	t.Helper()
	tokens, err := syntax.Tokenise(src)
	require.NoError(t, err)
	return syntax.Parse(tokens)
}

func firstStmt(t *testing.T, prog *syntax.Program) syntax.Stmt {
	t.Helper()
	require.Len(t, prog.Stmts, 1)
	return prog.Stmts[0]
}

func checkSource(t *testing.T, src string) []check.Issue {
	t.Helper()
	tokens, err := syntax.Tokenise(src)
	require.NoError(t, err)
	prog, err := syntax.Parse(tokens)
	require.NoError(t, err)
	return check.Check(prog)
}

// testExecAssertIR adapts an AssertStmt-oriented test callback to
// the lowered ir.Assert seam so tests can keep a compact statement-
// shaped assertion policy.
func testExecAssertIR(fn func(*syntax.AssertStmt, *Env) error) func(*ir.Assert, *Env) error {
	return func(a *ir.Assert, env *Env) error {
		return fn(&syntax.AssertStmt{IsRequire: a.IsRequire, Clause: astAssertClauseFromIR(a.Clause), Span: a.Span}, env)
	}
}

func requireAssertExprClause(tb testing.TB, stmt *syntax.AssertStmt) syntax.Expr {
	tb.Helper()
	return mustAssertExprClause(stmt)
}

func mustAssertExprClause(stmt *syntax.AssertStmt) syntax.Expr {
	clause, ok := stmt.Clause.(*syntax.AssertExprClause)
	if !ok {
		panic("assert clause is not AssertExprClause")
	}
	return clause.Expr
}

func astAssertClauseFromIR(clause ir.AssertClause) syntax.AssertClause {
	switch v := clause.(type) {
	case *ir.AssertExprClause:
		return &syntax.AssertExprClause{Expr: astExprFromIR(v.Expr)}
	case *ir.AssertCommandClause:
		args := make([]syntax.Expr, len(v.Args))
		for i, a := range v.Args {
			args[i] = astExprFromIR(a)
		}
		return &syntax.AssertCommandClause{
			Head:     v.Head,
			HeadSpan: v.HeadSpan,
			Args:     args,
			Negate:   v.Negate,
		}
	default:
		panic("astAssertClauseFromIR: unsupported clause")
	}
}

// astExprFromIR is the test-only inverse of the syntax->IR
// expression lowering path. Runtime and dump paths no longer
// reconstruct AST expressions from IR, but round-trip tests and
// compatibility adapters still need an explicit inverse to prove
// the lowering remains structure-preserving while the parser AST
// exists.
func astExprFromIR(expr ir.Expr) syntax.Expr {
	switch e := expr.(type) {
	case *ir.LiteralExpr:
		return &syntax.LiteralExpr{Text: e.Text, Quoted: e.Quoted, Span: e.Span}
	case *ir.VarRefExpr:
		return &syntax.VarRefExpr{Name: e.Name, Path: e.Path, Span: e.Span}
	case *ir.AdapterExpr:
		return &syntax.AdapterExpr{Adapter: e.Adapter, Name: e.Name, Path: e.Path, Span: e.Span}
	case *ir.ListExpr:
		elems := make([]syntax.Expr, len(e.Elems))
		for i, elem := range e.Elems {
			elems[i] = astExprFromIR(elem)
		}
		return &syntax.ListExpr{Elems: elems, Span: e.Span}
	case *ir.InterpStringExpr:
		segs := make([]syntax.InterpStringSegment, len(e.Segments))
		for i, seg := range e.Segments {
			segs[i].Literal = seg.Literal
			if seg.Expr != nil {
				segs[i].Expr = astExprFromIR(seg.Expr)
			}
		}
		return &syntax.InterpStringExpr{Segments: segs, Span: e.Span}
	case *ir.ThreadExpr:
		return &syntax.ThreadExpr{
			LHS:     astExprFromIR(e.LHS),
			Args:    astExprsFromIR(e.Args),
			PipePos: e.PipePos,
			Span:    e.Span,
		}
	case *ir.BinaryExpr:
		return &syntax.BinaryExpr{Left: astExprFromIR(e.Left), Op: e.Op, Right: astExprFromIR(e.Right), Span: e.Span}
	case *ir.UnaryExpr:
		return &syntax.UnaryExpr{Pred: e.Pred, Operand: astExprFromIR(e.Operand), Span: e.Span}
	case *ir.LogicalExpr:
		return &syntax.LogicalExpr{Op: e.Op, Left: astExprFromIR(e.Left), Right: astExprFromIR(e.Right), Span: e.Span}
	case *ir.NotExpr:
		return &syntax.NotExpr{Operand: astExprFromIR(e.Operand), Span: e.Span}
	case *ir.NegateExpr:
		return &syntax.NegateExpr{Operand: astExprFromIR(e.Operand), Span: e.Span}
	case *ir.PureCallExpr:
		return &syntax.PureCallExpr{Name: e.Name, Args: astExprsFromIR(e.Args), Span: e.Span}
	case *ir.MatchesExpr:
		return &syntax.MatchesExpr{
			Target: astExprFromIR(e.Target),
			Block:  astMatchesBlockFromIR(e.Block),
			Span:   e.Span,
		}
	default:
		panic(fmt.Sprintf("astExprFromIR: unhandled lowered expression type %T", expr))
	}
}

func astExprsFromIR(exprs []ir.Expr) []syntax.Expr {
	out := make([]syntax.Expr, len(exprs))
	for i, expr := range exprs {
		out[i] = astExprFromIR(expr)
	}
	return out
}

func astMatchesBlockFromIR(e *ir.MatchesBlockExpr) *syntax.MatchesBlockExpr {
	out := &syntax.MatchesBlockExpr{
		Entries:    make([]syntax.MatchEntry, len(e.Entries)),
		Exhaustive: e.Exhaustive,
		Span:       e.Span,
	}
	for i, entry := range e.Entries {
		out.Entries[i] = syntax.MatchEntry{
			Path:      entry.Path,
			Predicate: entry.Predicate,
			Span:      entry.Span,
		}
		if entry.Pattern != nil {
			out.Entries[i].Pattern = astExprFromIR(entry.Pattern)
		}
		if entry.SubBlock != nil {
			out.Entries[i].SubBlock = astMatchesBlockFromIR(entry.SubBlock)
		}
	}
	return out
}
