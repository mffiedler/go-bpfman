package ir

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"

// AssertClause is the lowered-runtime mirror of AssertClause.
// One Assert instruction carries one clause discriminator; the
// runtime keeps a single assertion lane and switches inside it.
type AssertClause interface {
	assertClauseIRNode()
}

type AssertExprClause struct {
	Expr Expr
}

type AssertCommandClause struct {
	Head     string
	HeadSpan source.Span
	Args     []Expr
	Negate   bool
}

func (*AssertExprClause) assertClauseIRNode()    {}
func (*AssertCommandClause) assertClauseIRNode() {}
