package shell

import (
	"fmt"
	"strconv"
)

// Expr is the sealed sum type for REPL expressions. The evaluator
// walks Expr nodes against an *Env to produce a [Value].
//
// The grammar:
//
//	expr    := primary | unary | binary
//	primary := LiteralExpr | VarRefExpr | AdapterExpr | CmdSubExpr
//	unary   := UnaryExpr (pred operand)
//	binary  := BinaryExpr (left op right)
type Expr interface {
	exprNode()
}

// LiteralExpr wraps a word or quoted-string token. Quoted records
// whether the operand came from a quoted literal; callers use it
// to preserve quoting semantics when rebuilding arguments for
// dispatch.
type LiteralExpr struct {
	Text   string
	Quoted bool
	Loc
}

// VarRefExpr is a variable reference with an optional field/index
// path. The referenced [Value] is resolved at evaluation time
// against the Session on the Env.
type VarRefExpr struct {
	Name string
	Path string
	Loc
}

// AdapterExpr is an adapter-decorated variable reference such as
// file:$var.path. Adapters only make sense in command-argument
// position; using one as an expression operand is a runtime error.
type AdapterExpr struct {
	Adapter string
	Name    string
	Path    string
	Loc
}

// CmdSubExpr is a command substitution [cmd args...]. Inner is the
// parsed inner program; at evaluation time the evaluator dispatches
// its single CommandStmt via Env.ExecSubstitution and returns the
// resulting Value.
type CmdSubExpr struct {
	Inner *Program
	Loc
}

// BinaryExpr is a two-operand comparison. Op is one of the
// recognised binary operators (word ops for textual comparison,
// symbol ops for numeric). Evaluation produces a BoolValue.
type BinaryExpr struct {
	Left  Expr
	Op    string
	Right Expr
	Loc
}

// UnaryExpr is a single-operand predicate. Pred is one of the
// recognised unary predicates (true, false, not-empty). Evaluation
// produces a BoolValue.
type UnaryExpr struct {
	Pred    string
	Operand Expr
	Loc
}

// PipeExpr is a value-threading composition: evaluate LHS to a
// Value, append that Value as the last argument of the command
// described by Args, and dispatch. The operator binds tighter than
// comparison operators but looser than the primary forms. Loc
// identifies the '|' token itself.
type PipeExpr struct {
	LHS  Expr
	Args []Expr
	Loc
}

func (*LiteralExpr) exprNode() {}
func (*VarRefExpr) exprNode()  {}
func (*AdapterExpr) exprNode() {}
func (*CmdSubExpr) exprNode()  {}
func (*BinaryExpr) exprNode()  {}
func (*UnaryExpr) exprNode()   {}
func (*PipeExpr) exprNode()    {}

// Env is the execution environment for the evaluator. Session is
// the variable and alias store; ExecCommand and ExecSubstitution
// dispatch commands to the REPL's shell and domain pipelines,
// differing only in output visibility and return-value
// requirements.
//
// A nil ExecCommand makes any top-level CommandStmt a runtime
// error; a nil ExecSubstitution makes any CmdSubExpr a runtime
// error. Tests that only exercise expression evaluation can leave
// both unset.
type Env struct {
	Session *Session

	// ExecCommand runs a top-level CommandStmt. The returned
	// Value may be nil; any output is visible on the CLI.
	ExecCommand func(args []Arg) (Value, error)

	// ExecSubstitution runs a command inside a cmd-sub
	// expression. Output is suppressed; the returned Value must
	// be non-nil or the evaluator reports an error.
	ExecSubstitution func(args []Arg) (Value, error)
}

// IsBinaryOp reports whether s is a recognised binary operator.
// Word operators compare textually (lexicographic); symbol
// operators compare numerically.
func IsBinaryOp(s string) bool {
	switch s {
	case "eq", "ne", "lt", "le", "gt", "ge",
		"==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// IsUnaryPred reports whether s is a recognised unary predicate in
// the expression grammar. The nil check is handled as a prefix
// verb in the assertion layer, not as a unary expression.
func IsUnaryPred(s string) bool {
	switch s {
	case "true", "false", "not-empty":
		return true
	}
	return false
}

func isNumericOp(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// EvalProgram executes each statement in prog against env in order.
// The first error halts evaluation and is returned to the caller,
// which decorates it with source-file context.
func EvalProgram(prog *Program, env *Env) error {
	for _, stmt := range prog.Stmts {
		if err := evalStmt(stmt, env); err != nil {
			return err
		}
	}
	return nil
}

func evalStmt(stmt Stmt, env *Env) error {
	switch s := stmt.(type) {
	case *LetStmt:
		val, err := EvalExpr(s.RHS, env)
		if err != nil {
			return err
		}
		if val.IsNil() {
			return locErrorf(s.Loc, "expression produced no result to assign")
		}
		env.Session.Set(s.Name, val)
		return nil
	case *IfStmt:
		return evalIfStmt(s, env)
	case *CommandStmt:
		return evalCommandStmt(s, env)
	default:
		return fmt.Errorf("unknown statement type %T", stmt)
	}
}

func evalIfStmt(s *IfStmt, env *Env) error {
	check := func(cond Expr) (bool, error) {
		v, err := EvalExpr(cond, env)
		if err != nil {
			return false, err
		}
		return AsBool(v)
	}
	runBody := func(body []Stmt) error {
		for _, stmt := range body {
			if err := evalStmt(stmt, env); err != nil {
				return err
			}
		}
		return nil
	}
	ok, err := check(s.Cond)
	if err != nil {
		return err
	}
	if ok {
		return runBody(s.Then)
	}
	for _, br := range s.Elifs {
		ok, err := check(br.Cond)
		if err != nil {
			return err
		}
		if ok {
			return runBody(br.Body)
		}
	}
	if len(s.Else) > 0 {
		return runBody(s.Else)
	}
	return nil
}

func evalCommandStmt(s *CommandStmt, env *Env) error {
	args, err := EvalArgs(s.Args, env)
	if err != nil {
		return err
	}
	if env.ExecCommand == nil {
		return locErrorf(s.Loc, "command execution is not configured")
	}
	_, err = env.ExecCommand(args)
	return err
}

// EvalExpr evaluates expr as a value-producing expression and
// returns its Value. Primary expressions (literals, variable
// references, command substitutions) produce a Value directly;
// binary and unary expressions combine their operands per their op
// or predicate. Adapter references are rejected — they only make
// sense as command arguments.
func EvalExpr(expr Expr, env *Env) (Value, error) {
	switch e := expr.(type) {
	case *LiteralExpr:
		return StringValue(e.Text), nil
	case *VarRefExpr:
		return resolveVarRefValue(e, env)
	case *AdapterExpr:
		return Value{}, locErrorf(e.Loc, "adapter %s:$%s cannot be used as an expression operand", e.Adapter, e.Name)
	case *CmdSubExpr:
		return dispatchCmdSub(e, env)
	case *PipeExpr:
		return dispatchPipe(e, env)
	case *BinaryExpr:
		return evalBinary(e, env)
	case *UnaryExpr:
		return evalUnary(e, env)
	default:
		return Value{}, fmt.Errorf("unhandled expression type %T", expr)
	}
}

// EvalArgs evaluates each Expr in exprs as a command argument and
// returns the resulting []Arg, suitable for dispatch. Command
// substitutions nested inside the list are evaluated via
// Env.ExecSubstitution, with their results wrapped as
// ScalarValueArg/StructuredValueArg according to their shape.
func EvalArgs(exprs []Expr, env *Env) ([]Arg, error) {
	out := make([]Arg, 0, len(exprs))
	for _, e := range exprs {
		a, err := evalArg(e, env)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func evalArg(expr Expr, env *Env) (Arg, error) {
	switch e := expr.(type) {
	case *LiteralExpr:
		if e.Quoted {
			return QuotedArg{Text: e.Text}, nil
		}
		return WordArg{Text: e.Text}, nil
	case *VarRefExpr:
		return resolveVarRefArg(e, env)
	case *AdapterExpr:
		return resolveAdapterArg(e, env)
	case *CmdSubExpr:
		val, err := dispatchCmdSub(e, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, locErrorf(e.Loc, "nested command substitution produced no value")
		}
		if val.IsStructured() {
			return StructuredValueArg{Value: val}, nil
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "nested command substitution: %v", err)
		}
		return ScalarValueArg{Text: s}, nil
	case *PipeExpr:
		val, err := dispatchPipe(e, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, locErrorf(e.Loc, "pipe produced no value")
		}
		if val.IsStructured() {
			return StructuredValueArg{Value: val}, nil
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "pipe: %v", err)
		}
		return ScalarValueArg{Text: s}, nil
	case *BinaryExpr, *UnaryExpr:
		return nil, locErrorf(exprLoc(expr), "comparison expression cannot be used as a command argument")
	default:
		return nil, locErrorf(exprLoc(expr), "cannot use %T as command argument", expr)
	}
}

func resolveVarRefValue(e *VarRefExpr, env *Env) (Value, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return Value{}, locErrorf(e.Loc, "undefined variable %q", e.Name)
	}
	if e.Path == "" {
		return v, nil
	}
	return v.LookupValue(e.Name, e.Path)
}

func resolveVarRefArg(e *VarRefExpr, env *Env) (Arg, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return nil, locErrorf(e.Loc, "undefined variable: %s", e.Name)
	}
	if e.Path == "" {
		if v.IsStructured() {
			return StructuredValueArg{Name: e.Name, Value: v}, nil
		}
		if v.IsNil() {
			return nil, locErrorf(e.Loc, "variable %s is null", e.Name)
		}
		s, err := v.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "variable %s: %v", e.Name, err)
		}
		return ScalarValueArg{Text: s}, nil
	}
	resolved, err := v.Lookup(e.Name, e.Path)
	if err != nil {
		return nil, err
	}
	s, err := resolved.Scalar()
	if err != nil {
		return nil, locErrorf(e.Loc, "variable %s.%s: %v", e.Name, e.Path, err)
	}
	return ScalarValueArg{Text: s}, nil
}

func resolveAdapterArg(e *AdapterExpr, env *Env) (Arg, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return nil, locErrorf(e.Loc, "undefined variable: %s", e.Name)
	}
	resolved := v
	if e.Path != "" {
		lv, err := v.LookupValue(e.Name, e.Path)
		if err != nil {
			return nil, err
		}
		resolved = lv
	}
	if resolved.IsNil() {
		return nil, locErrorf(e.Loc, "adapter %s: variable %s is null", e.Adapter, e.Name)
	}
	return AdapterArg{
		Adapter: e.Adapter,
		Name:    e.Name,
		Path:    e.Path,
		Value:   resolved,
	}, nil
}

// dispatchPipe evaluates a pipe expression by threading the LHS's
// Value into the command described by Args.  The LHS Value
// becomes the last element of the evaluated argument list so it
// matches the convention used by jq, json parse, file temp, and
// most shell-style "CMD ARGS VALUE" invocations.
func dispatchPipe(e *PipeExpr, env *Env) (Value, error) {
	if env.ExecSubstitution == nil {
		return Value{}, locErrorf(e.Loc, "pipe requires a substitution runner; none configured")
	}
	lhsVal, err := EvalExpr(e.LHS, env)
	if err != nil {
		return Value{}, err
	}
	if lhsVal.IsNil() {
		return Value{}, locErrorf(e.Loc, "pipe: left-hand side is null")
	}
	args, err := EvalArgs(e.Args, env)
	if err != nil {
		return Value{}, err
	}
	lhsArg, err := valueToArg(lhsVal)
	if err != nil {
		return Value{}, locErrorf(e.Loc, "pipe: %v", err)
	}
	return env.ExecSubstitution(append(args, lhsArg))
}

// valueToArg wraps a Value in the most specific Arg variant for
// the dispatch boundary: structured values stay structured,
// scalars become ScalarValueArg, nil is a caller problem.
func valueToArg(v Value) (Arg, error) {
	if v.IsNil() {
		return nil, fmt.Errorf("value is null")
	}
	if v.IsStructured() {
		return StructuredValueArg{Value: v}, nil
	}
	s, err := v.Scalar()
	if err != nil {
		return nil, err
	}
	return ScalarValueArg{Text: s}, nil
}

// dispatchCmdSub evaluates a command-substitution expression and
// returns the Value produced by the inner command. The inner
// program must contain exactly one CommandStmt — multiple
// statements or any non-command statement is a runtime error.
func dispatchCmdSub(e *CmdSubExpr, env *Env) (Value, error) {
	if env.ExecSubstitution == nil {
		return Value{}, locErrorf(e.Loc, "command substitution is not permitted in this context")
	}
	if e.Inner == nil || len(e.Inner.Stmts) == 0 {
		return Value{}, locErrorf(e.Loc, "empty command substitution")
	}
	if len(e.Inner.Stmts) != 1 {
		return Value{}, locErrorf(e.Loc, "command substitution must contain a single command")
	}
	cmd, ok := e.Inner.Stmts[0].(*CommandStmt)
	if !ok {
		return Value{}, locErrorf(e.Loc, "command substitution must contain a command, got %T", e.Inner.Stmts[0])
	}
	args, err := EvalArgs(cmd.Args, env)
	if err != nil {
		return Value{}, err
	}
	return env.ExecSubstitution(args)
}

func evalBinary(e *BinaryExpr, env *Env) (Value, error) {
	leftV, err := EvalExpr(e.Left, env)
	if err != nil {
		return Value{}, err
	}
	rightV, err := EvalExpr(e.Right, env)
	if err != nil {
		return Value{}, err
	}
	left, err := leftV.Scalar()
	if err != nil {
		return Value{}, locErrorf(e.Loc, "binary %s: left: %v", e.Op, err)
	}
	right, err := rightV.Scalar()
	if err != nil {
		return Value{}, locErrorf(e.Loc, "binary %s: right: %v", e.Op, err)
	}
	if isNumericOp(e.Op) {
		return evalNumericComparison(e.Op, left, right)
	}
	return evalTextualComparison(e.Op, left, right)
}

func evalUnary(e *UnaryExpr, env *Env) (Value, error) {
	operand, err := EvalExpr(e.Operand, env)
	if err != nil {
		return Value{}, err
	}
	switch e.Pred {
	case "not-empty":
		s, err := operand.Scalar()
		if err != nil {
			return Value{}, fmt.Errorf("not-empty: %w", err)
		}
		return BoolValue(s != ""), nil
	case "true":
		s, err := operand.Scalar()
		if err != nil {
			return Value{}, fmt.Errorf("true: %w", err)
		}
		return BoolValue(s == "true"), nil
	case "false":
		s, err := operand.Scalar()
		if err != nil {
			return Value{}, fmt.Errorf("false: %w", err)
		}
		return BoolValue(s == "false"), nil
	default:
		return Value{}, fmt.Errorf("unknown unary predicate %q", e.Pred)
	}
}

func evalTextualComparison(op, left, right string) (Value, error) {
	var pass bool
	switch op {
	case "eq":
		pass = left == right
	case "ne":
		pass = left != right
	case "lt":
		pass = left < right
	case "le":
		pass = left <= right
	case "gt":
		pass = left > right
	case "ge":
		pass = left >= right
	default:
		return Value{}, fmt.Errorf("unknown textual operator %q", op)
	}
	return BoolValue(pass), nil
}

func evalNumericComparison(op, left, right string) (Value, error) {
	a, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return Value{}, fmt.Errorf("left operand %q is not numeric", left)
	}
	b, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return Value{}, fmt.Errorf("right operand %q is not numeric", right)
	}
	var pass bool
	switch op {
	case "==":
		pass = a == b
	case "!=":
		pass = a != b
	case "<":
		pass = a < b
	case "<=":
		pass = a <= b
	case ">":
		pass = a > b
	case ">=":
		pass = a >= b
	default:
		return Value{}, fmt.Errorf("unknown numeric operator %q", op)
	}
	return BoolValue(pass), nil
}

// ExprFromArgs rebuilds a primary/unary/binary expression from a
// list of already-evaluated arguments. The assertion layer calls
// this to re-interpret a command's evaluated args as a comparison
// or predicate expression and then evaluate the whole thing via
// EvalExpr. The returned expression has zero Loc on every node
// because the original token positions are not available at this
// point in the pipeline.
func ExprFromArgs(args []Arg) (Expr, error) {
	switch len(args) {
	case 0:
		return nil, fmt.Errorf("empty expression")
	case 1:
		return argToPrimary(args[0])
	case 2:
		pred, ok := argAsUnaryPred(args[0])
		if !ok {
			return nil, fmt.Errorf("expected unary predicate as first operand, got %q", argDisplay(args[0]))
		}
		operand, err := argToPrimary(args[1])
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand}, nil
	case 3:
		op, ok := argAsBinaryOp(args[1])
		if !ok {
			return nil, fmt.Errorf("expected binary operator as middle operand, got %q", argDisplay(args[1]))
		}
		left, err := argToPrimary(args[0])
		if err != nil {
			return nil, err
		}
		right, err := argToPrimary(args[2])
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Left: left, Op: op, Right: right}, nil
	default:
		return nil, fmt.Errorf("expression has %d operands; expected 1 (primary), 2 (unary) or 3 (binary)", len(args))
	}
}

func argToPrimary(a Arg) (Expr, error) {
	switch v := a.(type) {
	case WordArg:
		return &LiteralExpr{Text: v.Text}, nil
	case QuotedArg:
		return &LiteralExpr{Text: v.Text, Quoted: true}, nil
	case ScalarValueArg:
		return &LiteralExpr{Text: v.Text}, nil
	case StructuredValueArg:
		return &VarRefExpr{Name: v.Name}, nil
	default:
		return nil, fmt.Errorf("cannot use %T as expression operand", a)
	}
}

func argAsUnaryPred(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok || !IsUnaryPred(w.Text) {
		return "", false
	}
	return w.Text, true
}

func argAsBinaryOp(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok || !IsBinaryOp(w.Text) {
		return "", false
	}
	return w.Text, true
}

func argDisplay(a Arg) string {
	switch v := a.(type) {
	case WordArg:
		return v.Text
	case QuotedArg:
		return v.Text
	case ScalarValueArg:
		return v.Text
	case StructuredValueArg:
		return "$" + v.Name
	default:
		return fmt.Sprintf("%T", a)
	}
}

// AsBool extracts a boolean from a Value. It succeeds only for
// OriginBool values; other origins return a type error. This is
// what if/elif/assert use to check condition expressions — there
// is no generic truthiness.
func AsBool(v Value) (bool, error) {
	if v.Kind() != OriginBool {
		return false, fmt.Errorf("condition is not a boolean (got %s); use a comparison or unary predicate", v.Kind())
	}
	b, ok := v.Raw().(bool)
	if !ok {
		return false, fmt.Errorf("condition has boolean origin but non-boolean value %T", v.Raw())
	}
	return b, nil
}

// exprLoc extracts the Loc embedded in any Expr variant. Used for
// error formatting where the caller has the Expr but not the
// concrete type.
func exprLoc(e Expr) Loc {
	switch v := e.(type) {
	case *LiteralExpr:
		return v.Loc
	case *VarRefExpr:
		return v.Loc
	case *AdapterExpr:
		return v.Loc
	case *CmdSubExpr:
		return v.Loc
	case *BinaryExpr:
		return v.Loc
	case *UnaryExpr:
		return v.Loc
	case *PipeExpr:
		return v.Loc
	}
	return Loc{}
}
