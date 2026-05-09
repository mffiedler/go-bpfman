// Static checks that run between parse and evaluation. The
// goal is to catch bugs that would otherwise surface at run
// time (and thus only after some side effects have fired) one
// pass earlier, when we still have the whole program in front
// of us. Today the only check is undefined-variable detection;
// the file is structured to make adding the next one (e.g.
// break/continue outside foreach, arithmetic on non-numeric
// literals) a small append rather than a refactor.
//
// The design borrows from go/types in spirit -- a separate
// pass over the AST that produces a list of issues -- but
// stays much smaller because our DSL has only one variable
// kind and no exported types. Each check uses Inspect for
// expression-level work; scope-bearing constructs (let, bind,
// foreach, def) drive the structural part by hand because
// pre-order traversal cannot express "define this name after
// processing the RHS, before walking the next statement".

package shell

import (
	"fmt"
	"strconv"
)

// Issue is one finding from a Check pass: a source location
// and a human-readable message. Multiple issues can be
// reported in a single Check invocation; severity is
// implicit (every Issue is an error today, but the field
// could grow if warnings become useful).
type Issue struct {
	Loc Loc
	Msg string
}

// Error renders the issue as 'line:col: message' so the
// driver layer can prepend a file path and emit the same
// shape parser/evaluator errors already use.
func (i Issue) Error() string {
	return fmt.Sprintf("%d:%d: %s", i.Loc.Line, i.Loc.Col, i.Msg)
}

// Check runs static analysis over prog and returns every
// issue it finds. Returning a slice rather than the first
// error lets callers report all problems at once instead of
// the user having to re-run after fixing each. An empty
// return slice means the program is clean by every check
// implemented today; future checks land here without changing
// the signature.
func Check(prog *Program) []Issue {
	c := &checker{defined: map[string]bool{}}
	c.walkStmts(prog.Stmts)
	c.checkJobLeaks(prog)
	c.checkArithmeticOperands(prog)
	return c.issues
}

// checker carries the rolling state for one Check pass. The
// defined map is the active set of variable names visible at
// the current point in the walk; foreach loop variables and
// def parameters are pushed on entry and popped on exit so
// they do not leak into following sibling statements at the
// same level.
type checker struct {
	defined map[string]bool
	issues  []Issue
}

// addIssue records an issue at loc with the given message.
// Pulled out so the formatter is in one place if the message
// shape changes.
func (c *checker) addIssue(loc Loc, format string, args ...any) {
	c.issues = append(c.issues, Issue{Loc: loc, Msg: fmt.Sprintf(format, args...)})
}

// walkStmts walks a statement list in source order. Defining
// statements (let, bind, foreach, def) update c.defined as a
// side effect of being walked; expression statements run
// their VarRef-usage check via checkExpr.
func (c *checker) walkStmts(stmts []Stmt) {
	for _, s := range stmts {
		c.walkStmt(s)
	}
}

// walkStmt dispatches on statement kind. The order of work
// inside each case matters: RHS expressions are checked before
// the binding name is added to defined, so 'let x = $x' on a
// previously-undefined x correctly reports $x undefined rather
// than silently letting the new binding shadow the lookup.
func (c *checker) walkStmt(s Stmt) {
	switch n := s.(type) {
	case *LetStmt:
		c.checkExpr(n.RHS)
		c.defined[n.Name] = true

	case *BindStmt:
		if n.Cmd != nil {
			for _, a := range n.Cmd.Args {
				c.checkExpr(a)
			}
		}
		if n.Primary != "" && n.Primary != "_" {
			c.defined[n.Primary] = true
		}
		if n.Rc != "" && n.Rc != "_" {
			c.defined[n.Rc] = true
		}

	case *ForEachStmt:
		c.checkExpr(n.List)
		// Loop variable is in scope inside the body only.
		// Save and restore the previous binding (if any) so
		// outer scope is preserved on exit.
		prev, had := c.defined[n.Name], c.defined[n.Name]
		c.defined[n.Name] = true
		c.walkStmts(n.Body)
		if had {
			c.defined[n.Name] = prev
		} else {
			delete(c.defined, n.Name)
		}

	case *DefStmt:
		// Parameters are visible inside the body. Save and
		// restore so nothing leaks to subsequent siblings.
		saved := make(map[string]bool, len(n.Params))
		for _, p := range n.Params {
			saved[p] = c.defined[p]
			c.defined[p] = true
		}
		c.walkStmts(n.Body)
		for p, prev := range saved {
			if prev {
				c.defined[p] = true
			} else {
				delete(c.defined, p)
			}
		}

	case *IfStmt:
		c.checkExpr(n.Cond)
		c.walkStmts(n.Then)
		for _, b := range n.Elifs {
			c.checkExpr(b.Cond)
			c.walkStmts(b.Body)
		}
		c.walkStmts(n.Else)

	case *DeferStmt:
		if n.Cmd != nil {
			for _, a := range n.Cmd.Args {
				c.checkExpr(a)
			}
		}

	case *RetryStmt:
		c.walkStmts(n.Body)
		c.checkExpr(n.Until)

	case *AssertStmt:
		c.checkExpr(n.Expr)

	case *ExprStmt:
		c.checkExpr(n.Expr)

	case *CommandStmt:
		for _, a := range n.Args {
			c.checkExpr(a)
		}

	case *BreakStmt, *ContinueStmt:
		// Leaves; nothing to check today.
	}
}

// checkExpr scans an expression subtree for VarRef usages
// against the current defined-set. Inspect is the right
// instrument here: an expression has no scoping of its own,
// so generic pre-order is exactly what we want.
func (c *checker) checkExpr(e Expr) {
	if e == nil {
		return
	}
	Inspect(e, func(n Node) bool {
		if v, ok := n.(*VarRefExpr); ok {
			if !c.defined[v.Name] {
				c.addIssue(v.Loc, "undefined variable: %s", v.Name)
			}
		}
		return true
	})
}

// checkJobLeaks reports started-but-never-managed jobs. A
// 'let X <- start ...' or 'guard X <- start ...' creates a
// job named X; a later 'kill $X', 'wait $X', or 'defer kill
// $X' marks it managed. An unmanaged job at script end is
// the static analogue of the runtime leak walk: same rule,
// caught one pass earlier so the user sees it before any
// side effects fire.
//
// The check is intentionally conservative: a 'kill $X' or
// 'wait $X' anywhere in the program counts, even inside a
// conditional branch the runtime might never enter. We
// prefer false-negatives (missed leaks the user sees at run
// time anyway) to false-positives (warning about scripts
// that work fine in practice). Sourced files are not
// analysed cross-file; each script is checked in isolation.
func (c *checker) checkJobLeaks(prog *Program) {
	type jobBinding struct {
		Name string
		Loc  Loc
	}

	var started []jobBinding
	managed := map[string]bool{}

	Inspect(prog, func(n Node) bool {
		switch s := n.(type) {
		case *BindStmt:
			if isStartCommand(s.Cmd) && s.Primary != "" && s.Primary != "_" {
				started = append(started, jobBinding{Name: s.Primary, Loc: s.Loc})
			}
		case *CommandStmt:
			if name := jobReferenceTarget(s); name != "" {
				managed[name] = true
			}
		case *DeferStmt:
			if s.Cmd != nil {
				if name := jobReferenceTarget(s.Cmd); name != "" {
					managed[name] = true
				}
			}
		}
		return true
	})

	for _, j := range started {
		if !managed[j.Name] {
			c.addIssue(j.Loc, "started job %q has no matching wait or kill", j.Name)
		}
	}
}

// isStartCommand reports whether cmd is a 'start ...' invocation.
// Used to recognise job-creating BindStmts; any other Cmd binds
// a non-job value and is not subject to leak analysis.
func isStartCommand(cmd *CommandStmt) bool {
	if cmd == nil || len(cmd.Args) == 0 {
		return false
	}
	lit, ok := cmd.Args[0].(*LiteralExpr)
	return ok && lit.Text == "start"
}

// checkArithmeticOperands flags literal operands of arithmetic
// operators (+, -, *, /, %) that cannot parse as numeric. The
// runtime evaluator already produces 'left operand "Z" is not
// numeric' for these; the static check pulls the diagnostic
// one pass earlier so an arithmetic typo in '${4 * Z}' or
// 'let r = A / B' surfaces before any side effect runs.
//
// Variable-reference operands are trusted (we cannot know
// their value at static time); only LiteralExpr operands are
// inspected. The numeric-vs-not test is strconv.ParseFloat
// which accepts the same lexical shapes the runtime
// arithmetic does (decimal integer, decimal float, scientific
// notation; hex via 0x prefix is rejected by ParseFloat but
// accepted by the runtime, so '0x1a + 1' would false-flag --
// match ParseFloat's behaviour by also trying ParseInt as a
// fallback so the static and runtime acceptances agree).
func (c *checker) checkArithmeticOperands(prog *Program) {
	Inspect(prog, func(n Node) bool {
		be, ok := n.(*BinaryExpr)
		if !ok || !isArithmeticOpText(be.Op) {
			return true
		}
		c.flagNonNumericLiteral(be.Left, be.Op)
		c.flagNonNumericLiteral(be.Right, be.Op)
		return true
	})
}

// flagNonNumericLiteral emits an issue when e is a literal
// whose text does not parse as a number. Other expression
// kinds (variable references, nested expressions, arithmetic
// sub-expressions) are trusted at static time.
func (c *checker) flagNonNumericLiteral(e Expr, op string) {
	lit, ok := e.(*LiteralExpr)
	if !ok {
		return
	}
	if isNumericLiteral(lit.Text) {
		return
	}
	c.addIssue(lit.Loc, "arithmetic %s: operand %q is not numeric", op, lit.Text)
}

// isNumericLiteral reports whether text is a literal the
// arithmetic evaluator will accept. Tries ParseFloat first
// (handles decimal integers, floats, scientific notation),
// then ParseInt with base 0 as a fallback (handles 0x... and
// 0... that ParseFloat rejects). Matches the runtime's de
// facto accepted shapes; if the runtime ever tightens this,
// the check tightens with it via the same helper.
func isNumericLiteral(text string) bool {
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseInt(text, 0, 64); err == nil {
		return true
	}
	return false
}

// jobReferenceTarget returns the variable name of a 'kill $X'
// or 'wait $X' command (the X), or "" if the command is not a
// kill or wait, or its target is not a simple VarRefExpr.
// Flag args (--signal=NAME, --grace=DUR) are skipped so 'kill
// --signal=USR1 $job' still picks up $job as the target.
func jobReferenceTarget(cmd *CommandStmt) string {
	if cmd == nil || len(cmd.Args) == 0 {
		return ""
	}
	lit, ok := cmd.Args[0].(*LiteralExpr)
	if !ok || (lit.Text != "kill" && lit.Text != "wait") {
		return ""
	}
	for _, arg := range cmd.Args[1:] {
		// Skip flag args; the target is the first non-flag.
		if l, ok := arg.(*LiteralExpr); ok && len(l.Text) >= 2 && l.Text[:2] == "--" {
			continue
		}
		if v, ok := arg.(*VarRefExpr); ok {
			return v.Name
		}
	}
	return ""
}
