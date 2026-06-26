package syntax

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"

// AssertClause is the syntax-owned body that follows an assert or
// require keyword. The parser always produces one AssertStmt; the
// clause discriminator records which assertion shape the user
// wrote without routing some forms through CommandStmt.
type AssertClause interface {
	assertClauseNode()
}

// AssertExprClause is the steady-state assertion form: any
// expression whose boolean value decides pass/fail.
type AssertExprClause struct {
	Expr Expr
}

// AssertCommandClause holds the command-status assertion forms:
// `assert ok CMD...` and `assert fail CMD...`. Head is the verb
// token; Args keep the command-style argument payload in
// expression form rather than embedding a statement node.
type AssertCommandClause struct {
	Head     string
	HeadSpan source.Span
	Args     []Expr
	Negate   bool
}

func (*AssertExprClause) assertClauseNode()    {}
func (*AssertCommandClause) assertClauseNode() {}
