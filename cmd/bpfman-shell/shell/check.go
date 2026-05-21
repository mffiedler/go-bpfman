// Static checks that run between parse and evaluation. The
// goal is to catch bugs that would otherwise surface at run
// time (and thus only after some side effects have fired) one
// pass earlier, when the whole program is still in front of
// us. The current set covers undefined variables, uncaptured
// background jobs, arithmetic on non-numeric literals or
// non-numeric variables, comparison-kind mismatches,
// break/continue outside foreach, builtin arity, kill-flag
// validation, and field-access typos against sealed kinds
// (Job, the captured-command-result kind, Program, Link).
//
// Like go/types, this is a separate pass over the AST that
// produces a list of issues; it stays much smaller because the
// DSL has a fixed kind enum and no user-extensible types. Each
// check uses Inspect for expression-level work; scope-bearing
// constructs (let, bind, foreach, def) drive the structural
// part by hand because pre-order traversal cannot express
// "define this name after processing the RHS, before walking
// the next statement".

package shell

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frobware/go-bpfman/internal/strdist"
)

// Issue is one finding from a Check pass: a source location
// and a human-readable message. Multiple issues can be
// reported in a single Check invocation; severity is
// implicit (every Issue is an error today, but the field
// could grow if warnings become useful).
type Issue struct {
	Span
	Msg string
}

// Error renders the issue as 'line:col: message' so the
// driver layer can prepend a file path and emit the same
// shape parser/evaluator errors already use.
func (i Issue) Error() string {
	return fmt.Sprintf("%d:%d: %s", i.Pos.Line, i.Pos.Col, i.Msg)
}

// Check runs static analysis over prog and returns every
// issue it finds. Returning a slice rather than the first
// error lets callers report all problems at once instead of
// the user having to re-run after fixing each. An empty
// return slice means the program is clean by every check
// implemented today; future checks land here without changing
// the signature.
func Check(prog *Program) []Issue {
	c := newChecker()
	c.walkStmts(prog.Stmts)
	c.checkJobLeaks(prog)
	c.checkArithmeticOperands(prog)
	c.checkComparisonOperands(prog)
	c.checkLoopExits(prog.Stmts, 0)
	c.checkBuiltinArity(prog)
	c.checkKillFlags(prog)
	return c.issues
}

// checker carries the rolling state for one Check pass. Variable
// state lives on a stack of frames that mirrors the runtime
// Session frame stack: defines write innermost, lookups walk
// outward, and a block-shaped construct pushes a frame for the
// duration of its body. Each checkFrame holds the per-variable
// shape and the verbatim RHS LiteralExpr for variables bound to a
// single literal, so type checks (notably arithmetic-operand
// validation) can inspect the original expression. Aliases are
// session-level and live outside the frame stack: `alias` /
// `unalias` are explicit user actions, not implicit by entering a
// block.
type checker struct {
	frames  []checkFrame
	aliases map[string]string
	issues  []Issue
	// defDepth counts the def bodies currently being walked. A
	// ReturnStmt is only valid when defDepth > 0. The depth is
	// non-zero inside nested blocks (if, foreach, eventually)
	// once we are inside a def, so a return tucked inside an
	// `if` branch of a def body is fine; a return at script top
	// level or inside an if-at-top-level is rejected.
	defDepth int
	// defs records the names registered via DefStmt so a bind
	// RHS whose head is a known def routes through the def's
	// open-shape inference rather than the pure-builtin /
	// envelope fallback. Defs are session-level declarations,
	// so a forward reference is not legal at runtime; the
	// checker mirrors that by adding the name when the DefStmt
	// is walked rather than pre-scanning.
	defs map[string]bool
}

// checkFrame is one entry on the checker's frame stack. defined
// carries the names introduced in this frame; shapes carries
// their inferred Shape (or KindShape(OriginUnknown) when not
// inferable); literals carries the bound RHS LiteralExpr for the
// arithmetic-on-literal validation, or nil when the binding did
// not come from a single literal.
type checkFrame struct {
	defined  map[string]bool
	shapes   map[string]Shape
	literals map[string]*LiteralExpr
}

func newChecker() *checker {
	return &checker{
		frames:  []checkFrame{newCheckFrame()},
		aliases: map[string]string{},
		defs:    map[string]bool{},
	}
}

func newCheckFrame() checkFrame {
	return checkFrame{
		defined:  map[string]bool{},
		shapes:   map[string]Shape{},
		literals: map[string]*LiteralExpr{},
	}
}

// define binds name into the innermost frame, shadowing any
// outer binding for the duration of the frame. Crossing a frame
// boundary creates a new shadowing binding rather than mutating
// an outer one -- the inner binding disappears when the frame is
// popped and the outer one becomes visible again. `_` is a
// discard slot at every binding site and contributes no binding.
func (c *checker) define(name string, shape Shape, lit *LiteralExpr) {
	if name == "_" {
		return
	}
	f := c.frames[len(c.frames)-1]
	f.defined[name] = true
	f.shapes[name] = shape
	if lit != nil {
		f.literals[name] = lit
	} else {
		delete(f.literals, name)
	}
}

// lookupDefined reports whether name resolves in any frame.
func (c *checker) lookupDefined(name string) bool {
	for i := len(c.frames) - 1; i >= 0; i-- {
		if c.frames[i].defined[name] {
			return true
		}
	}
	return false
}

// lookupShape returns the Shape for name found in the innermost
// frame that holds a binding. The second return value reports
// whether any frame had a shape entry; callers that find no
// entry treat the variable as an unsealed wildcard.
func (c *checker) lookupShape(name string) (Shape, bool) {
	for i := len(c.frames) - 1; i >= 0; i-- {
		if s, ok := c.frames[i].shapes[name]; ok {
			return s, true
		}
	}
	return Shape{}, false
}

// lookupLiteral returns the RHS LiteralExpr for name from the
// innermost frame that recorded one. Absence means the binding
// did not come from a single literal in any visible frame.
func (c *checker) lookupLiteral(name string) (*LiteralExpr, bool) {
	for i := len(c.frames) - 1; i >= 0; i-- {
		if lit, ok := c.frames[i].literals[name]; ok {
			return lit, true
		}
	}
	return nil, false
}

// withFrame pushes a fresh frame, runs fn, and pops in a defer
// so the pop runs on every exit path. The checker pushes frames
// only through withFrame so block scope is symmetric with the
// body's lexical extent.
func (c *checker) withFrame(fn func()) {
	c.frames = append(c.frames, newCheckFrame())
	defer func() {
		c.frames = c.frames[:len(c.frames)-1]
	}()
	fn()
}

// addIssue records an issue at span with the given message.
// Pulled out so the formatter is in one place if the message
// shape changes. Callers pass the offending node's full Span
// so the renderer can underline the relevant region rather than
// caret a single column.
func (c *checker) addIssue(span Span, format string, args ...any) {
	c.issues = append(c.issues, Issue{Span: span, Msg: fmt.Sprintf(format, args...)})
}

// inferExprShape returns the Shape a let RHS expression produces
// at static-check time. The Shape carries the OriginKind tag
// (so scalar / bool / known-record cases resolve directly) and
// any nested structure: a path-walk through a sealed parent
// returns the leaf field's Shape, a list-bound variable
// preserves its element shape, and unknown expressions return
// an unsealed wildcard.
func (c *checker) inferExprShape(e Expr) Shape {
	switch v := e.(type) {
	case *LiteralExpr:
		if v.Quoted {
			return KindShape(OriginScalar)
		}
		switch v.Text {
		case "true", "false":
			return KindShape(OriginBool)
		}
		return KindShape(OriginScalar)
	case *VarRefExpr:
		shape, ok := c.lookupShape(v.Name)
		if !ok {
			return Shape{Sealed: false, Kind: OriginUnknown}
		}
		if v.Path == "" {
			return shape
		}
		// Walk the Shape tree to find the leaf so a chained
		// let inherits the right shape. 'let q = $r.code'
		// gives q a Scalar shape; 'let head = $progs[0]'
		// gives head a Program shape (via the list's Elem).
		// An unsealed step returns Unknown, which still
		// propagates as a wildcard but disables nested
		// validation further down.
		for _, seg := range splitPathSegments(v.Path) {
			if seg.index {
				if shape.Elem != nil {
					shape = *shape.Elem
					continue
				}
				return Shape{Sealed: false, Kind: OriginUnknown}
			}
			if !shape.Sealed {
				return Shape{Sealed: false, Kind: OriginUnknown}
			}
			child, ok := shape.Fields[seg.name]
			if !ok {
				return Shape{Sealed: false, Kind: OriginUnknown}
			}
			shape = child
		}
		return shape
	case *BinaryExpr:
		switch v.Op {
		case "+", "-", "*", "/", "%":
			return KindShape(OriginScalar)
		}
		return KindShape(OriginBool)
	case *LogicalExpr, *NotExpr, *UnaryExpr:
		return KindShape(OriginBool)
	case *NegateExpr:
		return KindShape(OriginScalar)
	case *InterpStringExpr:
		return KindShape(OriginScalar)
	case *PureCallExpr:
		if pb, ok := LookupPureBuiltin(v.Name); ok {
			return pb.ReturnShape
		}
		return Shape{Sealed: false, Kind: OriginUnknown}
	}
	return Shape{Sealed: false, Kind: OriginUnknown}
}

// inferExprKind is sugar for inferExprShape(e).Kind. Existing
// kind-based checks (arithmetic, comparison) consult it without
// caring about the surrounding Shape; richer queries call
// inferExprShape directly.
func (c *checker) inferExprKind(e Expr) OriginKind {
	return c.inferExprShape(e).Kind
}

// inferBindShape returns the Shape a bind RHS CommandStmt
// produces in its primary slot.
//
// Resolution order:
//
//  1. The bind-shape registry (RegisterBindShape). cmd/bpfman-shell
//     wires every effectful builtin entry (start, fire, exec, wait,
//     kill, file, net, tempdir, ...) through this registry at init
//     time; the `bpfman` domain prefix registers itself the same
//     way. The registered BindShapeFn receives the args after the
//     command name so subcommand-aware shapes (net veth-pair ->
//     NetPair, net release -> result, net start -> Job; bpfman
//     program get -> Program, bpfman link list -> [Link], ...)
//     live next to the handler.
//
//  2. The pure-builtin registry (RegisterPureBuiltin). Pure entries
//     declare their return Shape there; the `<-` form remains a
//     compatibility spelling but `=` and `${...}` invoke the same
//     handler in expression position.
//
//  3. Everything else falls through to an external-subprocess
//     result envelope.
//
// The rc slot of a tuple bind is always result and is set by
// the caller.
func (c *checker) inferBindShape(cmd *CommandStmt) Shape {
	if cmd == nil || len(cmd.Args) == 0 {
		return Shape{Sealed: false, Kind: OriginUnknown}
	}
	first, ok := cmd.Args[0].(*LiteralExpr)
	if !ok || first.Quoted {
		return Shape{Sealed: false, Kind: OriginUnknown}
	}
	// Mirror the runtime's first-arg alias expansion (see
	// applyAlias in cmd/bpfman-shell/session.go) so an aliased
	// 'b program load file' resolves to bpfman's typed Program
	// shape rather than falling through to the external-command
	// result default.
	headText := first.Text
	if expanded, ok := c.aliases[headText]; ok {
		headText = expanded
	}
	if fn, ok := LookupBindShape(headText); ok {
		return fn(cmd.Args[1:])
	}
	if pb, ok := LookupPureBuiltin(headText); ok {
		return pb.ReturnShape
	}
	// Default: unknown first word runs as an external
	// subprocess via runExternalAsBind, which always returns
	// a result.
	return KindShape(OriginEnvelope)
}

// bindHeadPureBuiltin reports whether cmd's first word is a
// registered pure builtin (after alias expansion). The hint
// emitted by walkStmt cites the resolved name so a user reading
// the diagnostic sees the same spelling the registry would.
func bindHeadPureBuiltin(cmd *CommandStmt, aliases map[string]string) (string, bool) {
	if cmd == nil || len(cmd.Args) == 0 {
		return "", false
	}
	first, ok := cmd.Args[0].(*LiteralExpr)
	if !ok || first.Quoted {
		return "", false
	}
	head := first.Text
	if expanded, ok := aliases[head]; ok {
		head = expanded
	}
	if _, ok := LookupPureBuiltin(head); ok {
		return head, true
	}
	return "", false
}

// bindHeadDef reports whether cmd's first word names a def
// registered earlier in the walk. Aliases expand first because
// the runtime's first-arg alias expansion runs before def
// lookup. A matching def name takes precedence over both the
// pure-builtin rejection and the bind-shape registry: the
// def's `return EXPR` value is dynamic at preflight, so the
// primary slot binds with an open shape rather than the
// envelope fallback that mis-shapes def-bound primaries as
// commands' result envelopes.
func (c *checker) bindHeadDef(cmd *CommandStmt) bool {
	if cmd == nil || len(cmd.Args) == 0 {
		return false
	}
	first, ok := cmd.Args[0].(*LiteralExpr)
	if !ok || first.Quoted {
		return false
	}
	head := first.Text
	if expanded, ok := c.aliases[head]; ok {
		head = expanded
	}
	return c.defs[head]
}

// primaryNameForHint picks a placeholder for the bind-target
// slot in the diagnostic suggestion. The user's original name
// is reused when one was supplied; the tuple target is
// approximated by the primary slot's name when present;
// otherwise a generic "x" reads cleanly in the rewritten form
// the hint suggests.
func primaryNameForHint(n *BindStmt) string {
	switch {
	case n.Primary != "" && n.Primary != "_":
		return n.Primary
	case n.Rc != "" && n.Rc != "_":
		return n.Rc
	default:
		return "x"
	}
}

// walkStmts walks a statement list in source order. Defining
// statements (let, bind, foreach, def) call c.define as a side
// effect of being walked; expression statements run their
// VarRef-usage check via checkExpr.
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
		// Record the RHS literal when the binding is a single
		// LiteralExpr, quoted or not. The arithmetic check
		// consults the .Text and .Quoted fields so a quoted
		// string ('let s = "world"') and an unquoted
		// non-numeric token ('let s = bogus') both fire,
		// while a numeric literal stays clean.
		var lit *LiteralExpr
		if l, ok := n.RHS.(*LiteralExpr); ok {
			lit = l
		}
		c.define(n.Name, c.inferExprShape(n.RHS), lit)

	case *LetDestructureStmt:
		c.checkExpr(n.RHS)
		// Each non-'_' name becomes defined. Element shapes are
		// not inferred individually because the RHS could be any
		// list expression; only the binding existence matters
		// for downstream name-resolution.
		for _, name := range n.Names {
			c.define(name, Shape{Sealed: false, Kind: OriginUnknown}, nil)
		}

	case *BindStmt:
		if n.Cmd != nil {
			for _, a := range n.Cmd.Args {
				c.checkExpr(a)
			}
		}
		// Def dispatch precedes both the pure-builtin
		// rejection and the bind-shape lookup. A def name on
		// the bind RHS routes through callDefAsBind at runtime
		// no matter what other handler the same word might
		// match against, so the static checker must do the
		// same -- otherwise a def shadowing a pure-builtin
		// name would get rejected at preflight even though it
		// runs cleanly, and a def-bound primary would be
		// mis-shaped as the fallback envelope and reject any
		// field access the def's actual return shape supports.
		headIsDef := c.bindHeadDef(n.Cmd)
		if !headIsDef {
			// A '<-' bind on a pure builtin is rejected: pure
			// builtins produce no result envelope, so the rc
			// slot of '<-' (and the synthetic one a single-name
			// bind would discard) has nothing to carry. The '='
			// form is the only correct call shape in binding
			// position.
			if name, ok := bindHeadPureBuiltin(n.Cmd, c.aliases); ok {
				c.addIssue(n.Cmd.Span, "%s is a pure builtin; use 'let %s = %s ...' rather than '<-' (no result envelope is produced)", name, primaryNameForHint(n), name)
			}
		}
		// Tuple-bind on a bind-collect whose producer is a pure
		// builtin is rejected for the same reason: pure builtins
		// have no result envelope, so the rc slot would silently
		// collect synthetic OK envelopes. Single-bind is the
		// correct shape because it discards the rc list and
		// carries only the producer's value list. The same def-
		// precedence rule applies: a def producer routes through
		// callDefAsBind and is not a pure builtin even when its
		// name shadows one.
		if n.Collect != nil && n.Rc != "" && n.Rc != "_" && len(n.Collect.Body) > 0 {
			if last, ok := n.Collect.Body[len(n.Collect.Body)-1].(*CommandStmt); ok {
				if !c.bindHeadDef(last) {
					if name, ok := bindHeadPureBuiltin(last, c.aliases); ok {
						c.addIssue(last.Span, "%s is a pure builtin; tuple bind '(%s, %s)' is invalid in bind-collect because pure builtins produce no rc envelope; use single-bind 'let %s <- foreach ... { %s ... }' instead", name, n.Rc, primaryNameForHint(n), primaryNameForHint(n), name)
					}
				}
			}
		}
		// A def-bound primary is open: the return value's shape
		// is dynamic and the checker has no way to infer it
		// without flow analysis the language does not need.
		// Marking it as OriginUnknown matches what destructure
		// binding does for the same reason and unblocks field
		// access on def-returned structured values.
		var primaryShape Shape
		if headIsDef {
			primaryShape = Shape{Sealed: false, Kind: OriginUnknown}
		} else {
			primaryShape = c.inferBindShape(n.Cmd)
		}
		c.define(n.Primary, primaryShape, nil)
		c.define(n.Rc, KindShape(OriginEnvelope), nil)

	case *ForEachStmt:
		c.checkExpr(n.List)
		// Loop variables are in scope inside the body only.
		// The body runs inside a fresh frame so loop-var
		// bindings and any body-level `let` disappear at the
		// end of the loop. The checker has no notion of
		// iteration; one frame for the body is enough -- the
		// runtime allocates one frame per iteration but a
		// static walk cannot make use of the distinction.
		c.withFrame(func() {
			for _, name := range n.Names {
				c.define(name, Shape{Sealed: false, Kind: OriginUnknown}, nil)
			}
			c.walkStmts(n.Body)
		})

	case *DefStmt:
		// Register the def name BEFORE walking the body so a
		// recursive value-returning def -- a body that contains
		// `let v <- f` against itself -- gets the def-as-bind-
		// head routing during its own walk. Runtime def lookup
		// also resolves against the session's defs map at the
		// call site, which a recursive call hits after the
		// SetDef installation, so the checker matches what the
		// runtime does on the second and later iterations.
		c.defs[n.Name] = true
		// Parameters are visible inside the body and disappear
		// at end-of-def. The runtime allocates a fresh frame
		// per call; the checker walks the def body once with
		// its parameter frame in place. defDepth tracks whether
		// the walk is currently inside any def so the ReturnStmt
		// case can reject the keyword at the wrong nesting.
		c.defDepth++
		c.withFrame(func() {
			for _, p := range n.Params {
				c.define(p, Shape{Sealed: false, Kind: OriginUnknown}, nil)
			}
			c.walkStmts(n.Body)
		})
		c.defDepth--

	case *ReturnStmt:
		// `return EXPR` is only valid inside a def body. The
		// runtime carries a safety-net check at evalProgramBody
		// for paths the checker does not see (a return reached
		// only through a dynamic source, say), but the visible
		// shapes are caught here so the diagnostic lands at
		// check time, before any side effect fires. The
		// expression itself is checked even when the position
		// is wrong, so a script with both errors reports both.
		if c.defDepth == 0 {
			c.addIssue(n.Span, "return outside a def body")
		}
		if n.Expr != nil {
			c.checkExpr(n.Expr)
		}

	case *IfStmt:
		// Each branch body checks in its own frame: a `let`
		// inside one branch is invisible to subsequent
		// sibling branches and to the post-if scope. The
		// checker does not know which branch will run, so it
		// walks every branch independently; none contributes
		// bindings to the surrounding scope.
		c.checkExpr(n.Cond)
		c.withFrame(func() {
			c.walkStmts(n.Then)
		})
		for _, b := range n.Elifs {
			c.checkExpr(b.Cond)
			c.withFrame(func() {
				c.walkStmts(b.Body)
			})
		}
		if len(n.Else) > 0 {
			c.withFrame(func() {
				c.walkStmts(n.Else)
			})
		}

	case *DeferStmt:
		if n.Cmd != nil {
			for _, a := range n.Cmd.Args {
				c.checkExpr(a)
			}
		}

	case *EventuallyStmt:
		// The body checks in its own frame: body-level `let`
		// stays inside the attempt and is invisible to the
		// post-construct scope. The checker has no notion of
		// "attempt"; one frame for the body is enough, just
		// like foreach.
		c.withFrame(func() {
			c.walkStmts(n.Body)
		})

	case *AssertStmt:
		c.checkExpr(n.Expr)

	case *ExprStmt:
		c.checkExpr(n.Expr)

	case *CommandStmt:
		c.recordAlias(n)
		// `assert present $X.field` and `assert missing $X.field`
		// (and the `require` forms) test for the presence /
		// absence of a path in the value tree. The path is
		// allowed to name a field that the checker's inferred
		// shape says does not exist: that is precisely the
		// contract the missing predicate verifies. Skip
		// path-validity for the operand of those predicates.
		skipIdx := c.shapeProbeOperandIndex(n)
		for i, a := range n.Args {
			if i == skipIdx {
				continue
			}
			c.checkExpr(a)
		}

	case *BreakStmt, *ContinueStmt:
		// Leaves; nothing to check today.
	}
}

// shapeProbeOperandIndex returns the index of the operand for an
// `assert present`, `assert missing` (or `require` variant),
// and `not`-prefixed forms, or -1 when the command is not one of
// these shape-probe predicates. The operand is intentionally
// excluded from VarRef path-validity checking because the
// predicate's contract is to test the very thing the checker
// would reject as unknown -- the absence of a field path from
// the value tree.
func (c *checker) shapeProbeOperandIndex(n *CommandStmt) int {
	if len(n.Args) < 2 {
		return -1
	}
	head, ok := n.Args[0].(*LiteralExpr)
	if !ok {
		return -1
	}
	if head.Text != "assert" && head.Text != "require" {
		return -1
	}
	verbIdx := 1
	verb, ok := n.Args[verbIdx].(*LiteralExpr)
	if !ok {
		return -1
	}
	if verb.Text == "not" && len(n.Args) >= 3 {
		verbIdx = 2
		verb, ok = n.Args[verbIdx].(*LiteralExpr)
		if !ok {
			return -1
		}
	}
	switch verb.Text {
	case "present", "missing":
		if verbIdx+1 < len(n.Args) {
			return verbIdx + 1
		}
	}
	return -1
}

// recordAlias detects an `alias NAME = VALUE` command and
// records the mapping so subsequent bind-RHS inference can see
// through aliases. The runtime expands aliases at the first-arg
// boundary (applyAlias in cmd/bpfman-shell); the checker mirrors
// that expansion at static-check time so a script that aliases
// 'b = bpfman' and writes 'guard p <- b program get $pid' gets
// the same OriginProgram inference it would with the alias
// expanded inline. 'unalias NAME' removes the mapping.
//
// The grammar is the same one cmd/bpfman-shell/session.go's
// replAlias enforces: 'alias NAME = VALUE' with exactly four
// argument slots and a literal '=' in slot two. Anything else
// is left to the runtime checks; recordAlias is best-effort.
func (c *checker) recordAlias(n *CommandStmt) {
	if len(n.Args) == 0 {
		return
	}
	head, ok := n.Args[0].(*LiteralExpr)
	if !ok {
		return
	}
	switch head.Text {
	case "alias":
		if len(n.Args) != 4 {
			return
		}
		name, nameOK := n.Args[1].(*LiteralExpr)
		eq, eqOK := n.Args[2].(*LiteralExpr)
		val, valOK := n.Args[3].(*LiteralExpr)
		if !nameOK || !eqOK || !valOK || eq.Text != "=" {
			return
		}
		c.aliases[name.Text] = val.Text
	case "unalias":
		for _, a := range n.Args[1:] {
			if lit, ok := a.(*LiteralExpr); ok {
				delete(c.aliases, lit.Text)
			}
		}
	}
}

// checkExpr scans an expression subtree for VarRef usages
// against the current defined-set, plus path-validity against
// any sealed kinds the checker has inferred for the variable.
// Inspect is the right instrument here: an expression has no
// scoping of its own, so generic pre-order is exactly what we
// want.
func (c *checker) checkExpr(e Expr) {
	if e == nil {
		return
	}
	Inspect(e, func(n Node) bool {
		if v, ok := n.(*VarRefExpr); ok {
			if !c.lookupDefined(v.Name) {
				c.addIssue(v.Span, "undefined variable: %s", v.Name)
				return true
			}
			c.checkVarRefPath(v)
		}
		return true
	})
}

// checkVarRefPath validates v's path against the variable's
// inferred Shape, descending field by field. The walker stops at
// the first segment that is not in a sealed parent's field set
// and emits a frame underlining the whole varref with a "did you
// mean ..." suggestion derived via internal/strdist.
//
// Index segments ([N]) descend through Shape.Elem when the
// Shape is a list; lists with no Elem (or non-list parents)
// permit indexing without comment because we cannot disprove the
// shape. Once the walk lands on an unsealed Shape every
// remaining segment is accepted -- the checker has lost
// visibility into nested structure and refusing to walk further
// would produce false positives.
func (c *checker) checkVarRefPath(v *VarRefExpr) {
	if v.Path == "" {
		return
	}
	current, ok := c.lookupShape(v.Name)
	if !ok {
		return
	}
	currentName := v.Name
	currentKind := current.Kind

	for _, seg := range splitPathSegments(v.Path) {
		if seg.index {
			if current.Elem != nil {
				current = *current.Elem
				currentKind = current.Kind
				continue
			}
			// Either this Shape isn't a list, or its Elem
			// is not registered. Descend into Unknown so we
			// stop trying to validate further fields.
			current = Shape{Sealed: false, Kind: OriginUnknown}
			currentKind = OriginUnknown
			continue
		}
		if !current.Sealed {
			return
		}
		if len(current.Fields) == 0 {
			c.addIssue(v.Span, "%s has kind %s; field access is not valid", currentName, currentKind)
			return
		}
		child, ok := current.Fields[seg.name]
		if !ok {
			c.addIssue(v.Span, "%s",
				unknownFieldMsg(currentName, currentKind, seg.name, current.Fields))
			return
		}
		current = child
		currentName = currentName + "." + seg.name
		currentKind = child.Kind
	}
}

// pathSegment is a single step inside a varref path: either a
// dotted field name or a "[N]" index step.
type pathSegment struct {
	name  string
	index bool
}

// splitPathSegments parses a varref path into its component
// steps. The grammar is the same one the lexer accepts inside
// "${name.path}" / "$name[0].field": dotted names alternate with
// "[N]" index steps, and the leading dot (if any) is implicit
// because VarRefExpr.Path is stored without it.
func splitPathSegments(path string) []pathSegment {
	var out []pathSegment
	i := 0
	for i < len(path) {
		if path[i] == '.' {
			i++
			continue
		}
		if path[i] == '[' {
			j := i + 1
			for j < len(path) && path[j] != ']' {
				j++
			}
			out = append(out, pathSegment{index: true})
			if j < len(path) {
				j++
			}
			i = j
			continue
		}
		j := i
		for j < len(path) && path[j] != '.' && path[j] != '[' {
			j++
		}
		out = append(out, pathSegment{name: path[i:j]})
		i = j
	}
	return out
}

// unknownFieldMsg renders the "no field X (valid: ...; did you
// mean Y?)" message used when a path segment misses the parent
// Shape's field set. The kind label is included only when it is
// informative -- nested record types (Program.Record, Link.Record,
// etc.) carry OriginUnknown for their kind tag because the
// reflector does not cross-link Go types to OriginKinds, and
// "X has kind unknown" reads as noise; the field set is the
// useful information. The valid list is sorted so error
// rendering is stable; the suggestion list comes from
// internal/strdist.
func unknownFieldMsg(name string, kind OriginKind, seg string, fields map[string]Shape) string {
	valid := slices.Sorted(maps.Keys(fields))
	suggestions := strdist.Nearest(seg, valid, 3)
	var msg string
	if kind == OriginUnknown {
		msg = fmt.Sprintf("%s has no field %q (valid: %s)",
			name, seg, strings.Join(valid, ", "))
	} else {
		msg = fmt.Sprintf("%s has kind %s; field %q does not exist (valid: %s)",
			name, kind, seg, strings.Join(valid, ", "))
	}
	if len(suggestions) > 0 {
		msg += "; did you mean " + strings.Join(quoteAll(suggestions), ", ") + "?"
	}
	return msg
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
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
		Span
	}

	var started []jobBinding
	managed := map[string]bool{}

	Inspect(prog, func(n Node) bool {
		switch s := n.(type) {
		case *BindStmt:
			if isStartCommand(s.Cmd) && s.Primary != "" && s.Primary != "_" {
				started = append(started, jobBinding{Name: s.Primary, Span: s.Span})
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
			c.addIssue(j.Span, "started job %q has no matching wait or kill", j.Name)
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
		c.flagNonNumericOperand(be.Left, be.Op)
		c.flagNonNumericOperand(be.Right, be.Op)
		return true
	})
}

// flagNonNumericOperand emits an issue when e is statically
// known to be non-numeric. Three sources of evidence are
// consulted in turn: a literal whose text fails the numeric
// parser; a varref whose kind cannot represent a number
// (Bool, Job, the captured-result kind, the bpfman record
// kinds, Map, Null); and a varref bound to a literal whose
// text is non-numeric (Scalar kind alone is ambiguous because
// "world" and "5" are both Scalar -- the recorded RHS text
// resolves the ambiguity). Variables of OriginUnknown or
// path-walked Scalars are trusted at static time because the
// runtime value is genuinely opaque.
func (c *checker) flagNonNumericOperand(e Expr, op string) {
	if lit, ok := e.(*LiteralExpr); ok {
		if isNumericLiteral(lit.Text) {
			return
		}
		c.addIssue(lit.Span, "arithmetic %s: operand %q is not numeric", op, lit.Text)
		return
	}
	v, ok := e.(*VarRefExpr)
	if !ok {
		return
	}
	kind := c.inferExprKind(v)
	switch kind {
	case OriginScalar, OriginUnknown:
		// Scalars may be numeric or not; consult the recorded
		// literal RHS when the varref is a bare name with no
		// path walk and was bound to a single LiteralExpr.
		// A quoted string ('let s = "world"') is always
		// non-numeric; an unquoted token gets the same
		// numeric-parser test isNumericLiteral applies to
		// arithmetic-on-literal operands.
		if v.Path == "" {
			if lit, ok := c.lookupLiteral(v.Name); ok {
				if lit.Quoted || !isNumericLiteral(lit.Text) {
					c.addIssue(v.Span, "arithmetic %s: %s is %q, not a number", op, v.Name, lit.Text)
				}
			}
		}
	case OriginBool:
		c.addIssue(v.Span, "arithmetic %s: %s is a boolean, not a number", op, v.Name)
	case OriginNull:
		c.addIssue(v.Span, "arithmetic %s: %s is null, not a number", op, v.Name)
	default:
		c.addIssue(v.Span, "arithmetic %s: %s has kind %s, not a number", op, v.Name, kind)
	}
}

// checkComparisonOperands reports comparisons whose operand
// kinds are known and incompatible. The runtime's evalCompare
// already errors on a Bool-vs-Scalar comparison or on any
// non-scalar operand; this static check catches the same
// shapes earlier so the user does not have to run the script
// to find them. Only sealed kinds with a clear mismatch are
// flagged; one operand of OriginUnknown or OriginScalar
// (without a known literal text) silences the check because
// the runtime types are genuinely ambiguous.
func (c *checker) checkComparisonOperands(prog *Program) {
	Inspect(prog, func(n Node) bool {
		be, ok := n.(*BinaryExpr)
		if !ok || isArithmeticOpText(be.Op) {
			return true
		}
		if !isComparisonOp(be.Op) {
			return true
		}
		l := c.inferExprKind(be.Left)
		r := c.inferExprKind(be.Right)
		if l == OriginUnknown || r == OriginUnknown {
			return true
		}
		if comparable, mismatch := classifyComparison(l, r, be.Op); !comparable {
			c.addIssue(be.Span, "binary %s: %s", be.Op, mismatch)
		}
		return true
	})
}

// isComparisonOp reports whether op is one of the binary
// comparison operators evalCompare handles: equality, ordering,
// or their textual aliases. Logical operators (and, or) and
// arithmetic operators are handled by their own checks.
func isComparisonOp(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// classifyComparison returns whether two kinds can be compared
// under op, and a human-readable explanation when they cannot.
// The rules mirror evalCompare: non-scalar operands cannot be
// compared; Bool supports only == and !=; Scalar-vs-Bool is a
// kind mismatch; otherwise the operands are comparable. The
// caller has already filtered out OriginUnknown so this
// classifier never sees a wildcard.
func classifyComparison(l, r OriginKind, op string) (ok bool, msg string) {
	if !isScalarLikeKind(l) {
		return false, fmt.Sprintf("left side has kind %s; only scalars (numbers, strings, booleans) can be compared with %s", l, op)
	}
	if !isScalarLikeKind(r) {
		return false, fmt.Sprintf("right side has kind %s; only scalars (numbers, strings, booleans) can be compared with %s", r, op)
	}
	if l != r {
		return false, fmt.Sprintf("cannot compare %s to %s; coerce explicitly", l, r)
	}
	if l == OriginBool && op != "==" && op != "!=" {
		return false, fmt.Sprintf("booleans support only == and !=, not %s", op)
	}
	return true, ""
}

// isScalarLikeKind reports whether kind names a value that the
// comparison evaluator accepts. Scalars (numbers, strings),
// Booleans, and Null are scalar-like; record kinds, jobs,
// command results, lists, and maps are not.
func isScalarLikeKind(k OriginKind) bool {
	switch k {
	case OriginScalar, OriginBool, OriginNull:
		return true
	}
	return false
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
		case *EventuallyStmt:
			// Eventually is not an iteration construct: break
			// and continue are only valid inside a foreach,
			// nested or otherwise. Pass through depth so a
			// nested foreach inside the body still admits
			// break/continue against itself.
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
				c.addIssue(n.Span, "'break' outside any foreach loop")
			}
		case *ContinueStmt:
			if depth == 0 {
				c.addIssue(n.Span, "'continue' outside any foreach loop")
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
		// fire's positional shape is (KIND, SENTINEL, ACK) but its
		// flags (--count=N, --waves=K) frequently interpolate (e.g.
		// --count=$n), which makes them non-LiteralExpr args that
		// nonFlagArgCount cannot recognise as flags. Strict static
		// counting would reject every interpolated invocation. The
		// runtime handler enforces the exact positional/flag shape;
		// the v2 work in PLAN-fire-builtin.md hoists kind-name and
		// NeedsBinary validation into the checker, which is where a
		// flag-aware arity check belongs.
		"fire": {min: 1, max: -1},
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
			c.addIssue(cmd.Span, "%s: expected at least %d argument(s), got %d", head.Text, spec.min, got)
		case spec.max >= 0 && got > spec.max:
			c.addIssue(cmd.Span, "%s: expected at most %d argument(s), got %d", head.Text, spec.max, got)
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
					c.addIssue(lit.Span, "kill --signal: unknown signal %q", name)
				}
			case strings.HasPrefix(lit.Text, "--grace="):
				dur := strings.TrimPrefix(lit.Text, "--grace=")
				if _, err := time.ParseDuration(dur); err != nil {
					c.addIssue(lit.Span, "kill --grace: %v", err)
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
