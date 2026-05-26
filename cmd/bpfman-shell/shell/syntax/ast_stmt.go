package syntax

import (
	"time"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"
)

// Program is the root of a parsed source unit: an ordered sequence
// of statements with the source location of the first token.
type Program struct {
	Stmts []Stmt
	source.Span
}

// Stmt is the sealed sum type for statements. Embedding Node
// lets every Stmt be passed to Inspect without an explicit
// type assertion at the call site.
type Stmt interface {
	Node
	stmtNode()
}

// LetStmt binds the result of evaluating RHS to Name. Name is
// guaranteed to be a valid identifier by the parser.
type LetStmt struct {
	Name string
	RHS  Expr
	source.Span
}

// LetDestructureStmt binds the positional elements of a list
// expression to a fixed-length name list: `let (a b) = EXPR`,
// `let (a _ c) = EXPR`. RHS must evaluate to a list of length
// len(Names); each non-'_' name binds to its element. The parser
// rejects single-name parenthesised forms, comma separators,
// duplicate real names, and all-underscore name lists; runtime
// errors fire when RHS is not a list or its length does not match.
type LetDestructureStmt struct {
	Names []string
	RHS   Expr
	source.Span
}

// BindStmt runs Cmd and binds its primary result, and optionally
// its result envelope. Two surface forms parse here:
//
//	let NAME <- CMD              => Primary=NAME, Rc=""
//	let (RC NAME) <- CMD         => Primary=NAME, Rc=RC
//	guard NAME <- CMD            => same shape, Guard=true
//	guard (RC NAME) <- CMD       => same shape, Guard=true
//
// A third surface form, bind-collect, sets Collect instead of Cmd:
//
//	let NAME <- foreach X in LIST { BODY }
//	let (RC NAME) <- foreach X in LIST { BODY }
//	guard NAME <- foreach X in LIST { BODY }
//	guard (RC NAME) <- foreach X in LIST { BODY }
//
// BODY is iterated once per element of LIST; the body's last
// statement must be a CommandStmt and is executed as the bind's
// producer. The producer's primary value (and rc envelope, when
// the tuple form is used) is accumulated into a list per
// iteration. continue skips a particular iteration's
// accumulation; break terminates iteration and binds the
// partial collection. Guard semantics carry: if the outer bind
// is a guard, a non-ok envelope on any iteration halts the
// whole collect via GuardFailure with no binding.
//
// Exactly one of Cmd and Collect is non-nil. "_"
// as a target name discards that slot. Single-name binding
// always names the primary; tuple binding names rc then primary,
// matching section 6.2 of the design.
type BindStmt struct {
	Primary string
	Rc      string
	Cmd     *CommandStmt
	Collect *ForEachStmt
	Guard   bool
	source.Span
}

// IfBranch pairs a condition expression with a block body. Used
// for the primary branch and each elif.
type IfBranch struct {
	Cond Expr
	Body []Stmt
	source.Span
}

// IfStmt is an if-elif-else conditional.
type IfStmt struct {
	Cond  Expr
	Then  []Stmt
	Elifs []IfBranch
	Else  []Stmt
	source.Span
}

// CommandStmt is a plain command invocation. The first element of
// Args names the command.
type CommandStmt struct {
	Args []Expr
	source.Span
}

// ExprStmt is an expression appearing in statement position. It is
// only produced inside a command substitution "[EXPR]" when the
// bracketed content parses as an expression. At the program level
// the parser never emits ExprStmt; the only statement forms are the
// named ones above plus a plain CommandStmt.
type ExprStmt struct {
	Expr Expr
	source.Span
}

// ForEachStmt iterates a block over the elements of a list. At
// eval time List is evaluated to a Value; it must be a structured
// list, and each element is bound across Names in the Session for
// the duration of its iteration. The bindings are body-scoped:
// any prior binding of a name is restored on exit and a name that
// did not exist before the loop disappears again.
type ForEachStmt struct {
	Names []string
	List  Expr
	Body  []Stmt
	source.Span
}

// BreakStmt terminates the nearest enclosing ForEachStmt. Outside
// a loop it is a runtime error.
type BreakStmt struct{ source.Span }

// ContinueStmt skips the remainder of the current ForEachStmt
// iteration and advances to the next element. Outside a loop it
// is a runtime error.
type ContinueStmt struct{ source.Span }

// PollStmt retries a block until it reaches the end without an
// explicit retry, or until its timeout budget is exhausted.
type PollStmt struct {
	Timeout time.Duration
	Every   time.Duration
	Body    []Stmt
	source.Span
}

// RetryStmt requests another poll attempt. It is valid inside a
// poll body and inside helper defs that are executed from a poll.
// Message is optional; Unless is optional and gates the retry so
// `retry unless EXPR` becomes a no-op when EXPR is true.
type RetryStmt struct {
	Message Expr
	Unless  Expr
	source.Span
}

// AssertStmt is the syntax-owned assertion statement.
type AssertStmt struct {
	IsRequire bool
	Clause    AssertClause
	source.Span
}

// DeferStmt registers a cleanup command for the enclosing defer
// scope.
type DeferStmt struct {
	Cmd *CommandStmt
	source.Span
}

// DefStmt declares a user-defined command.
type DefStmt struct {
	Name   string
	Params []string
	Body   []Stmt
	source.Span
}

// ReturnStmt is the value-publishing exit from a def body.
type ReturnStmt struct {
	Expr Expr
	source.Span
}

func (*LetStmt) stmtNode()            {}
func (*LetDestructureStmt) stmtNode() {}
func (*BindStmt) stmtNode()           {}
func (*DeferStmt) stmtNode()          {}
func (*IfStmt) stmtNode()             {}
func (*CommandStmt) stmtNode()        {}
func (*ExprStmt) stmtNode()           {}
func (*ForEachStmt) stmtNode()        {}
func (*BreakStmt) stmtNode()          {}
func (*ContinueStmt) stmtNode()       {}
func (*PollStmt) stmtNode()           {}
func (*RetryStmt) stmtNode()          {}
func (*DefStmt) stmtNode()            {}
func (*ReturnStmt) stmtNode()         {}
func (*AssertStmt) stmtNode()         {}
