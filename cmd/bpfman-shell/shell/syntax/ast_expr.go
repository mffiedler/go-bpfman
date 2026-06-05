package syntax

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"

// Expr is the sealed sum type for shell expressions.
type Expr interface {
	Node
	exprNode()
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

type InterpStringExpr struct {
	Segments []InterpStringSegment
	source.Span
}

type InterpStringSegment struct {
	Literal string
	Expr    Expr
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

type ThreadExpr struct {
	LHS     Expr
	Args    []Expr
	PipePos source.Pos
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

type ListExpr struct {
	Elems []Expr
	source.Span
}

type RecordField struct {
	Name string
	Expr Expr
	source.Span
}

type RecordExpr struct {
	Fields []RecordField
	source.Span
}

func (*LiteralExpr) exprNode()      {}
func (*VarRefExpr) exprNode()       {}
func (*AdapterExpr) exprNode()      {}
func (*InterpStringExpr) exprNode() {}
func (*BinaryExpr) exprNode()       {}
func (*UnaryExpr) exprNode()        {}
func (*ThreadExpr) exprNode()       {}
func (*LogicalExpr) exprNode()      {}
func (*NotExpr) exprNode()          {}
func (*NegateExpr) exprNode()       {}
func (*PureCallExpr) exprNode()     {}
func (*MatchesExpr) exprNode()      {}
func (*ListExpr) exprNode()         {}
func (*RecordExpr) exprNode()       {}
