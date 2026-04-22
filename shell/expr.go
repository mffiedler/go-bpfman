package shell

import (
	"fmt"
	"strconv"
)

// Expr is the sealed sum type for REPL expressions. An expression
// evaluates against a Session and produces a Value.
//
// The grammar:
//
//	expr    := primary | unary | binary
//	primary := LiteralExpr | VarRefExpr
//	unary   := UnaryExpr (pred operand)
//	binary  := BinaryExpr (left op right)
//
// Command substitution is deferred to a later phase; once added, it
// will become another primary variant.
type Expr interface {
	isExpr()
}

// LiteralExpr wraps a word or quoted string. The Text is the
// post-expansion text of the operand; the Quoted flag records whether
// it came from a quoted token (reserved for future use).
type LiteralExpr struct {
	Text   string
	Quoted bool
}

// VarRefExpr is a variable reference with an optional field/index
// path. The referenced Value is resolved at evaluation time against
// the Session. An empty Path means the bare value (structured or
// scalar) is returned as-is.
type VarRefExpr struct {
	Name string
	Path string
}

// BinaryExpr is a two-operand comparison. Op is one of the recognised
// binary operators (word ops for textual comparison, symbol ops for
// numeric). Evaluation produces a BoolValue.
type BinaryExpr struct {
	Left  Expr
	Op    string
	Right Expr
}

// UnaryExpr is a single-operand predicate. Pred is one of the
// recognised unary predicates (not-empty, true, false).
// Evaluation produces a BoolValue.
type UnaryExpr struct {
	Pred    string
	Operand Expr
}

// CmdSubExpr is a command substitution [cmd args...]. At evaluation
// time the inner args are dispatched via the CmdRunner and the
// command's returned Value becomes the expression's value.
type CmdSubExpr struct {
	InnerArgs []Arg
}

func (*LiteralExpr) isExpr() {}
func (*VarRefExpr) isExpr()  {}
func (*BinaryExpr) isExpr()  {}
func (*UnaryExpr) isExpr()   {}
func (*CmdSubExpr) isExpr()  {}

// CmdRunner dispatches a command substitution and returns the
// resulting Value. Implementations bridge into the REPL's shell
// builtin and domain-command pipelines.
type CmdRunner func(innerArgs []Arg) (Value, error)

// IsBinaryOp reports whether s is a recognised binary operator.
// Word operators compare textually (lexicographic); symbol operators
// compare numerically.
func IsBinaryOp(s string) bool {
	switch s {
	case "eq", "ne", "lt", "le", "gt", "ge",
		"==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// IsUnaryPred reports whether s is a recognised unary predicate in
// the expression grammar. The predicates that take a single value
// operand: not-empty, true, false. The nil check is not here: it
// takes a bare variable name rather than a value expression, and is
// handled as a prefix verb in the assertion layer.
func IsUnaryPred(s string) bool {
	switch s {
	case "true", "false", "not-empty":
		return true
	}
	return false
}

// isNumericOp reports whether op uses numeric comparison semantics.
// Symbol operators are numeric; word operators are textual.
func isNumericOp(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// ParseExpr parses a sequence of already-expanded arguments into an
// Expr. The recognised shapes, in priority order:
//
//   - [lhs op rhs] with IsBinaryOp(op) → BinaryExpr
//   - [pred operand] with IsUnaryPred(pred) → UnaryExpr
//   - [operand] → primary (LiteralExpr or VarRefExpr depending on Arg)
//
// Returns a descriptive error for empty input or shapes that do not
// match. Command substitution is not yet a grammar variant.
func ParseExpr(args []Arg) (Expr, error) {
	switch len(args) {
	case 0:
		return nil, fmt.Errorf("empty expression")
	case 1:
		return parsePrimary(args[0])
	case 2:
		pred, ok := unaryPredText(args[0])
		if !ok {
			return nil, fmt.Errorf("expected unary predicate as first operand, got %q", argDisplay(args[0]))
		}
		operand, err := parsePrimary(args[1])
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand}, nil
	case 3:
		op, ok := binaryOpText(args[1])
		if !ok {
			return nil, fmt.Errorf("expected binary operator as middle operand, got %q", argDisplay(args[1]))
		}
		left, err := parsePrimary(args[0])
		if err != nil {
			return nil, err
		}
		right, err := parsePrimary(args[2])
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Left: left, Op: op, Right: right}, nil
	default:
		return nil, fmt.Errorf("expression has %d operands; expected 1 (primary), 2 (unary) or 3 (binary)", len(args))
	}
}

// parsePrimary turns a single Arg into a LiteralExpr, VarRefExpr, or
// CmdSubExpr. ScalarValueArg from a path-resolved variable reference
// becomes a LiteralExpr (the text is the resolved scalar). A bare
// StructuredValueArg becomes a VarRefExpr with no Path so that the
// evaluator can recover the full Value. A CmdSubArg becomes a
// CmdSubExpr carrying the expanded inner args.
func parsePrimary(a Arg) (Expr, error) {
	switch v := a.(type) {
	case WordArg:
		return &LiteralExpr{Text: v.Text}, nil
	case QuotedArg:
		return &LiteralExpr{Text: v.Text, Quoted: true}, nil
	case ScalarValueArg:
		return &LiteralExpr{Text: v.Text}, nil
	case StructuredValueArg:
		return &VarRefExpr{Name: v.Name, Path: ""}, nil
	case CmdSubArg:
		return &CmdSubExpr{InnerArgs: v.InnerArgs}, nil
	default:
		return nil, fmt.Errorf("cannot use %T as expression operand", a)
	}
}

// unaryPredText returns the predicate name if the arg is a word that
// matches a known unary predicate. Variable args do not match.
func unaryPredText(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok {
		return "", false
	}
	if IsUnaryPred(w.Text) {
		return w.Text, true
	}
	return "", false
}

// binaryOpText returns the operator text if the arg is a word that
// matches a known binary operator.
func binaryOpText(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok {
		return "", false
	}
	if IsBinaryOp(w.Text) {
		return w.Text, true
	}
	return "", false
}

// argDisplay produces a user-facing string for an Arg.
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

// Eval evaluates the expression against the given Session and returns
// the resulting Value. Comparisons and unary predicates produce
// Values with OriginBool. Command substitutions are dispatched via
// the supplied CmdRunner; a nil runner makes CmdSubExpr a runtime
// error.
func Eval(e Expr, session *Session, runner CmdRunner) (Value, error) {
	switch x := e.(type) {
	case *LiteralExpr:
		return StringValue(x.Text), nil

	case *VarRefExpr:
		v, ok := session.Get(x.Name)
		if !ok {
			return Value{}, fmt.Errorf("undefined variable %q", x.Name)
		}
		if x.Path == "" {
			return v, nil
		}
		return v.LookupValue(x.Name, x.Path)

	case *UnaryExpr:
		return evalUnary(x, session, runner)

	case *BinaryExpr:
		return evalBinary(x, session, runner)

	case *CmdSubExpr:
		if runner == nil {
			return Value{}, fmt.Errorf("command substitution is not permitted in this context")
		}
		return runner(x.InnerArgs)

	default:
		return Value{}, fmt.Errorf("unhandled expression type %T", e)
	}
}

// evalUnary evaluates a unary predicate. Returns a BoolValue.
func evalUnary(e *UnaryExpr, session *Session, runner CmdRunner) (Value, error) {
	operand, err := Eval(e.Operand, session, runner)
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

// evalBinary evaluates a binary comparison. Returns a BoolValue.
func evalBinary(e *BinaryExpr, session *Session, runner CmdRunner) (Value, error) {
	leftV, err := Eval(e.Left, session, runner)
	if err != nil {
		return Value{}, err
	}
	rightV, err := Eval(e.Right, session, runner)
	if err != nil {
		return Value{}, err
	}
	left, err := leftV.Scalar()
	if err != nil {
		return Value{}, fmt.Errorf("binary %s: left: %w", e.Op, err)
	}
	right, err := rightV.Scalar()
	if err != nil {
		return Value{}, fmt.Errorf("binary %s: right: %w", e.Op, err)
	}
	if isNumericOp(e.Op) {
		return evalNumericComparison(e.Op, left, right)
	}
	return evalTextualComparison(e.Op, left, right)
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

// AsBool extracts a boolean from a Value. It succeeds only for
// OriginBool values; other origins (scalars, structured) return a
// type error. This is what if/elif/assert use to check condition
// expressions — there is no generic truthiness.
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
