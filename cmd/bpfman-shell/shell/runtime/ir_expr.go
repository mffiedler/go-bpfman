package runtime

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/ir"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
)

func evalIRExpr(expr ir.Expr, env *Env) (Value, error) {
	switch e := expr.(type) {
	case *ir.LiteralExpr:
		return literalValueParts(e.Text, e.Quoted), nil
	case *ir.VarRefExpr:
		return resolveVarRefValueParts(e.Name, e.Path, e.Span, env)
	case *ir.AdapterExpr:
		return Value{}, syntax.SpanErrorf(e.Span, "adapter %s:$%s cannot be used as an expression operand", e.Adapter, e.Name)
	case *ir.ListExpr:
		out := make([]any, 0, len(e.Elems))
		origins := make([]any, 0, len(e.Elems))
		hasOrigin := false
		for _, elem := range e.Elems {
			v, err := evalIRExpr(elem, env)
			if err != nil {
				return Value{}, err
			}
			out = append(out, v.Raw())
			o := v.Origin()
			origins = append(origins, o)
			if o != nil {
				hasOrigin = true
			}
		}
		list := ValueFromAny(out)
		if hasOrigin {
			list = list.withOrigin(origins, semantics.OriginUnknown)
		}
		return list, nil
	case *ir.InterpStringExpr:
		var b strings.Builder
		for _, seg := range e.Segments {
			if seg.Expr == nil {
				b.WriteString(seg.Literal)
				continue
			}
			v, err := evalIRExpr(seg.Expr, env)
			if err != nil {
				return Value{}, err
			}
			s, err := RenderCompact(v)
			if err != nil {
				return Value{}, syntax.SpanErrorf(irExprSpan(seg.Expr), "interpolation: %v", err)
			}
			b.WriteString(s)
		}
		return StringValue(b.String()), nil
	case *ir.ThreadExpr:
		threadSpan := irThreadDiagSpan(e)
		if env.ExecBind == nil {
			return Value{}, syntax.SpanErrorf(threadSpan, "'|>' is only valid where commands can run; not available in this context")
		}
		args, err := evalIRArgs(e.Args, env)
		if err != nil {
			return Value{}, err
		}
		lhsArg, err := evalIRArg(e.LHS, env)
		if err != nil {
			return Value{}, syntax.SpanErrorf(threadSpan, "thread: %v", err)
		}
		result, err := env.ExecBind(append(args, lhsArg), e.Span)
		if err != nil {
			return Value{}, syntax.FrameAt(threadSpan, err)
		}
		if !result.Rc.OK {
			if result.Rc.Stderr != "" {
				return Value{}, syntax.SpanErrorf(threadSpan, "thread: command failed (exit %d): %s", result.Rc.Code, result.Rc.Stderr)
			}
			return Value{}, syntax.SpanErrorf(threadSpan, "thread: command failed (exit %d)", result.Rc.Code)
		}
		return result.Primary, nil
	case *ir.BinaryExpr:
		leftV, err := evalIRExpr(e.Left, env)
		if err != nil {
			return Value{}, err
		}
		rightV, err := evalIRExpr(e.Right, env)
		if err != nil {
			return Value{}, err
		}
		if isArithmeticOpText(e.Op) {
			left, err := leftV.Scalar()
			if err != nil {
				return Value{}, syntax.SpanErrorf(e.Span, "binary %s: left: %v", e.Op, err)
			}
			right, err := rightV.Scalar()
			if err != nil {
				return Value{}, syntax.SpanErrorf(e.Span, "binary %s: right: %v", e.Op, err)
			}
			v, err := evalArithmetic(e.Op, left, right)
			if err != nil {
				return Value{}, syntax.SpanErrorf(e.Span, "%v", err)
			}
			return v, nil
		}
		return evalCompare(e.Op, leftV, rightV, e.Span)
	case *ir.UnaryExpr:
		operand, err := evalIRExpr(e.Operand, env)
		if err != nil {
			return Value{}, err
		}
		switch e.Pred {
		case "not-empty":
			if operand.IsNil() || operand.IsNull() {
				return BoolValue(false), nil
			}
			switch x := operand.Raw().(type) {
			case string:
				return BoolValue(x != ""), nil
			case []any:
				return BoolValue(len(x) > 0), nil
			case map[string]any:
				return BoolValue(len(x) > 0), nil
			case json.Number:
				f, ferr := x.Float64()
				if ferr != nil {
					return Value{}, syntax.SpanErrorf(e.Span, "not-empty: %v", ferr)
				}
				return BoolValue(f != 0), nil
			case float64:
				return BoolValue(x != 0), nil
			case bool:
				return BoolValue(x), nil
			default:
				return BoolValue(true), nil
			}
		default:
			return Value{}, syntax.SpanErrorf(e.Span, "unknown unary predicate %q", e.Pred)
		}
	case *ir.LogicalExpr:
		leftV, err := evalIRExpr(e.Left, env)
		if err != nil {
			return Value{}, err
		}
		leftB, err := AsBool(leftV)
		if err != nil {
			return Value{}, syntax.SpanErrorf(e.Span, "%s: left: %v", e.Op, err)
		}
		switch e.Op {
		case "and":
			if !leftB {
				return BoolValue(false), nil
			}
		case "or":
			if leftB {
				return BoolValue(true), nil
			}
		default:
			return Value{}, syntax.SpanErrorf(e.Span, "unknown logical operator %q", e.Op)
		}
		rightV, err := evalIRExpr(e.Right, env)
		if err != nil {
			return Value{}, err
		}
		rightB, err := AsBool(rightV)
		if err != nil {
			return Value{}, syntax.SpanErrorf(e.Span, "%s: right: %v", e.Op, err)
		}
		return BoolValue(rightB), nil
	case *ir.NotExpr:
		v, err := evalIRExpr(e.Operand, env)
		if err != nil {
			return Value{}, err
		}
		b, err := AsBool(v)
		if err != nil {
			return Value{}, syntax.SpanErrorf(e.Span, "not: %v", err)
		}
		return BoolValue(!b), nil
	case *ir.NegateExpr:
		v, err := evalIRExpr(e.Operand, env)
		if err != nil {
			return Value{}, err
		}
		s, err := v.Scalar()
		if err != nil {
			return Value{}, syntax.SpanErrorf(e.Span, "negate: %v", err)
		}
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return Value{}, syntax.SpanErrorf(e.Span, "negate: operand %q is not numeric", s)
		}
		return numericValue(-x), nil
	case *ir.PureCallExpr:
		if env.ExecBind == nil {
			return Value{}, syntax.SpanErrorf(e.Span, "%s: pure-builtin calls require an active command dispatcher", e.Name)
		}
		args := make([]Arg, 0, len(e.Args)+1)
		args = append(args, WordArg{Text: e.Name, Span: e.Span})
		for _, a := range e.Args {
			arg, err := evalIRArg(a, env)
			if err != nil {
				return Value{}, syntax.SpanErrorf(irExprSpan(a), "%s: %v", e.Name, err)
			}
			args = append(args, arg)
		}
		result, err := env.ExecBind(args, e.Span)
		if err != nil {
			return Value{}, syntax.FrameAt(e.Span, err)
		}
		if !result.Rc.OK {
			if result.Rc.Stderr != "" {
				return Value{}, syntax.SpanErrorf(e.Span, "%s: %s", e.Name, result.Rc.Stderr)
			}
			return Value{}, syntax.SpanErrorf(e.Span, "%s: call failed (exit %d)", e.Name, result.Rc.Code)
		}
		return result.Primary, nil
	case *ir.MatchesExpr:
		result, err := evalIRMatchesExprDetails(e, env)
		if err != nil {
			return Value{}, err
		}
		return BoolValue(result.Matched), nil
	default:
		panic(fmt.Sprintf("evalIRExpr: unhandled lowered expression type %T", expr))
	}
}

// EvalIRExpr evaluates one lowered expression against env. This is
// the public lowered-expression counterpart to EvalExpr and is used
// by the remaining non-runtime bridges such as assertion handling.
func EvalIRExpr(expr ir.Expr, env *Env) (Value, error) {
	return evalIRExpr(expr, env)
}

// EvalIRArgs evaluates each lowered expression in exprs as a
// command argument and returns the resulting []Arg, suitable for
// dispatch.
func EvalIRArgs(exprs []ir.Expr, env *Env) ([]Arg, error) {
	return evalIRArgs(exprs, env)
}

func evalIRArgs(exprs []ir.Expr, env *Env) ([]Arg, error) {
	out := make([]Arg, 0, len(exprs))
	for _, expr := range exprs {
		a, err := evalIRArg(expr, env)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func evalIRArg(expr ir.Expr, env *Env) (Arg, error) {
	switch e := expr.(type) {
	case *ir.LiteralExpr:
		if e.Quoted {
			return QuotedArg{Text: e.Text, Span: e.Span}, nil
		}
		return WordArg{Text: e.Text, Span: e.Span}, nil
	case *ir.VarRefExpr:
		return resolveVarRefArgParts(e.Name, e.Path, e.Span, env)
	case *ir.AdapterExpr:
		return resolveAdapterArgParts(e.Adapter, e.Name, e.Path, e.Span, env)
	case *ir.ThreadExpr:
		val, err := evalIRExpr(e, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, syntax.SpanErrorf(irThreadDiagSpan(e), "thread produced no value")
		}
		return valueToArg(val, e.Span)
	default:
		v, err := evalIRExpr(expr, env)
		if err != nil {
			return nil, err
		}
		if v.IsNil() {
			return nil, syntax.SpanErrorf(irExprSpan(expr), "parenthesised expression produced no value")
		}
		return valueToArg(v, irExprSpan(expr))
	}
}

func evalIRMatchesBlock(e *ir.MatchesBlockExpr, env *Env) (resolvedMatchesBlock, error) {
	out := resolvedMatchesBlock{
		Entries:    make([]resolvedMatchEntry, 0, len(e.Entries)),
		Exhaustive: e.Exhaustive,
		Span:       e.Span,
	}
	for _, entry := range e.Entries {
		ent := resolvedMatchEntry{
			Path:      entry.Path,
			Predicate: entry.Predicate,
			Span:      entry.Span,
		}
		switch {
		case entry.SubBlock != nil:
			sub, err := evalIRMatchesBlock(entry.SubBlock, env)
			if err != nil {
				return resolvedMatchesBlock{}, err
			}
			ent.SubBlock = &sub
		case entry.Predicate != "":
			// nothing to evaluate
		default:
			v, err := evalIRExpr(entry.Pattern, env)
			if err != nil {
				return resolvedMatchesBlock{}, syntax.SpanErrorf(entry.Span, "matches entry %q: %v", entry.Path, err)
			}
			ent.Value = v
		}
		out.Entries = append(out.Entries, ent)
	}
	return out, nil
}

func irExprSpan(expr ir.Expr) source.Span {
	switch e := expr.(type) {
	case *ir.LiteralExpr:
		return e.Span
	case *ir.VarRefExpr:
		return e.Span
	case *ir.AdapterExpr:
		return e.Span
	case *ir.ListExpr:
		return e.Span
	case *ir.InterpStringExpr:
		return e.Span
	case *ir.ThreadExpr:
		return e.Span
	case *ir.BinaryExpr:
		return e.Span
	case *ir.UnaryExpr:
		return e.Span
	case *ir.LogicalExpr:
		return e.Span
	case *ir.NotExpr:
		return e.Span
	case *ir.NegateExpr:
		return e.Span
	case *ir.PureCallExpr:
		return e.Span
	case *ir.MatchesExpr:
		return e.Span
	default:
		panic(fmt.Sprintf("irExprSpan: unhandled lowered expression type %T", expr))
	}
}

func irThreadDiagSpan(e *ir.ThreadExpr) source.Span {
	if e.PipePos != (source.Pos{}) {
		return source.Span{Pos: e.PipePos, End: e.PipePos}
	}
	return e.Span
}
