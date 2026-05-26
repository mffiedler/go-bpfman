package ir

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"

// Expr is the lowered-expression sum type carried by Eval,
// BuildArgs, and Assert. It mirrors the surface expression
// families the runtime still needs after statement lowering,
// but it is distinct from the parser AST so the lowered engine
// can retire its dependency on Expr incrementally.
type Expr interface {
	irExprNode()
}

type LiteralExpr struct {
	Text   string
	Quoted bool
	source.Span
}

type VarRefExpr struct {
	Name string
	Path string
	source.Span
}

type AdapterExpr struct {
	Adapter string
	Name    string
	Path    string
	source.Span
}

type ListExpr struct {
	Elems []Expr
	source.Span
}

type InterpStringExpr struct {
	Segments []InterpStringSegment
	source.Span
}

type InterpStringSegment struct {
	Literal string
	Expr    Expr
}

type ThreadExpr struct {
	LHS     Expr
	Args    []Expr
	PipePos source.Pos
	source.Span
}

type BinaryExpr struct {
	Left  Expr
	Op    string
	Right Expr
	source.Span
}

type UnaryExpr struct {
	Pred    string
	Operand Expr
	source.Span
}

type LogicalExpr struct {
	Op          string
	Left, Right Expr
	source.Span
}

type NotExpr struct {
	Operand Expr
	source.Span
}

type NegateExpr struct {
	Operand Expr
	source.Span
}

type PureCallExpr struct {
	Name string
	Args []Expr
	source.Span
}

type MatchesExpr struct {
	Target Expr
	Block  *MatchesBlockExpr
	source.Span
}

type MatchesBlockExpr struct {
	Entries    []MatchEntry
	Exhaustive bool
	source.Span
}

type MatchEntry struct {
	Path      string
	Pattern   Expr
	SubBlock  *MatchesBlockExpr
	Predicate string
	source.Span
}

func (*LiteralExpr) irExprNode()      {}
func (*VarRefExpr) irExprNode()       {}
func (*AdapterExpr) irExprNode()      {}
func (*ListExpr) irExprNode()         {}
func (*InterpStringExpr) irExprNode() {}
func (*ThreadExpr) irExprNode()       {}
func (*BinaryExpr) irExprNode()       {}
func (*UnaryExpr) irExprNode()        {}
func (*LogicalExpr) irExprNode()      {}
func (*NotExpr) irExprNode()          {}
func (*NegateExpr) irExprNode()       {}
func (*PureCallExpr) irExprNode()     {}
func (*MatchesExpr) irExprNode()      {}
