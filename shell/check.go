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
	"strings"
	"time"
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
	c.checkLoopExits(prog.Stmts, 0)
	c.checkBuiltinArity(prog)
	c.checkKillFlags(prog)
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

// checkLoopExits walks stmts with a foreach-depth counter,
// flagging 'break' or 'continue' that appear at depth 0
// (outside any enclosing foreach). Retry blocks do not count
// as foreach for this purpose -- the runtime errBreak /
// errContinue are caught only by ForEachStmt's evaluator -- so
// retry bodies inherit the caller's depth. Def bodies reset
// the depth: a def is a callable unit, and break/continue
// inside the body but not inside a foreach within the def
// body is wrong even if the def itself is later called from
// inside a foreach.
func (c *checker) checkLoopExits(stmts []Stmt, depth int) {
	for _, s := range stmts {
		switch n := s.(type) {
		case *ForEachStmt:
			c.checkLoopExits(n.Body, depth+1)
		case *RetryStmt:
			c.checkLoopExits(n.Body, depth)
		case *IfStmt:
			c.checkLoopExits(n.Then, depth)
			for _, b := range n.Elifs {
				c.checkLoopExits(b.Body, depth)
			}
			c.checkLoopExits(n.Else, depth)
		case *DefStmt:
			c.checkLoopExits(n.Body, 0)
		case *BreakStmt:
			if depth == 0 {
				c.addIssue(n.Loc, "'break' outside any foreach loop")
			}
		case *ContinueStmt:
			if depth == 0 {
				c.addIssue(n.Loc, "'continue' outside any foreach loop")
			}
		}
	}
}

// checkBuiltinArity flags shape errors on the async-job
// builtins the runtime documents as taking specific argument
// counts. Static catches typos like 'kill --signa=USR1 $p'
// (--signa is not a flag, so the kill ends up with two
// non-flag args) one pass before the runtime does. Flag args
// (anything starting with '--') are skipped so the count
// reflects only the positional args the runtime cares about.
//
// Coupling: the builtin names and their arities are
// duplicated here from cmd/bpfman-shell. The set is small and
// stable; if a new lifecycle verb lands, the entry adds in
// one place. Driver-side dispatch and static check stay in
// step via convention rather than a shared registry.
func (c *checker) checkBuiltinArity(prog *Program) {
	type aritySpec struct {
		min, max int // -1 max means unbounded
	}
	specs := map[string]aritySpec{
		"start": {min: 1, max: -1}, // command and optional args
		"wait":  {min: 1, max: 1},  // exactly one $job
		"kill":  {min: 1, max: 1},  // exactly one $job (after flags)
		"jobs":  {min: 0, max: 0},
		"reap":  {min: 0, max: 0},
	}
	Inspect(prog, func(n Node) bool {
		cmd, ok := n.(*CommandStmt)
		if !ok || len(cmd.Args) == 0 {
			return true
		}
		head, ok := cmd.Args[0].(*LiteralExpr)
		if !ok {
			return true
		}
		spec, known := specs[head.Text]
		if !known {
			return true
		}
		got := nonFlagArgCount(cmd.Args[1:])
		switch {
		case got < spec.min:
			c.addIssue(cmd.Loc, "%s: expected at least %d argument(s), got %d", head.Text, spec.min, got)
		case spec.max >= 0 && got > spec.max:
			c.addIssue(cmd.Loc, "%s: expected at most %d argument(s), got %d", head.Text, spec.max, got)
		}
		return true
	})
}

// checkKillFlags validates --signal=NAME and --grace=DUR
// values on kill invocations. The runtime catches the same
// errors when the kill builtin actually runs; static checking
// surfaces the typo before any side effect. NAME is matched
// against the same fixed signal set the runtime accepts
// (TERM, KILL, INT, QUIT, HUP, USR1, USR2, STOP, CONT) with
// optional 'SIG' prefix and case-insensitive lookup. DUR is
// fed to time.ParseDuration which matches the runtime's
// acceptance.
func (c *checker) checkKillFlags(prog *Program) {
	Inspect(prog, func(n Node) bool {
		cmd, ok := n.(*CommandStmt)
		if !ok || len(cmd.Args) == 0 {
			return true
		}
		head, ok := cmd.Args[0].(*LiteralExpr)
		if !ok || head.Text != "kill" {
			return true
		}
		for _, arg := range cmd.Args[1:] {
			lit, ok := arg.(*LiteralExpr)
			if !ok {
				continue
			}
			switch {
			case strings.HasPrefix(lit.Text, "--signal="):
				name := strings.TrimPrefix(lit.Text, "--signal=")
				if !isKnownSignalName(name) {
					c.addIssue(lit.Loc, "kill --signal: unknown signal %q", name)
				}
			case strings.HasPrefix(lit.Text, "--grace="):
				dur := strings.TrimPrefix(lit.Text, "--grace=")
				if _, err := time.ParseDuration(dur); err != nil {
					c.addIssue(lit.Loc, "kill --grace: %v", err)
				}
			}
		}
		return true
	})
}

// nonFlagArgCount returns the number of args that are not
// '--'-prefixed flag literals. Flag args (--signal=NAME,
// --grace=DUR, ...) are skipped so an arity check counts
// only the positional args the runtime cares about.
func nonFlagArgCount(args []Expr) int {
	n := 0
	for _, a := range args {
		if lit, ok := a.(*LiteralExpr); ok && len(lit.Text) >= 2 && lit.Text[:2] == "--" {
			continue
		}
		n++
	}
	return n
}

// isKnownSignalName reports whether name matches one of the
// signals the runtime kill builtin accepts. Case-insensitive,
// optional 'SIG' prefix. Mirrors signalFromName in
// cmd/bpfman-shell/job.go; if that list grows, this list
// grows the same way.
func isKnownSignalName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	upper = strings.TrimPrefix(upper, "SIG")
	switch upper {
	case "TERM", "KILL", "INT", "QUIT", "HUP", "USR1", "USR2", "STOP", "CONT":
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
