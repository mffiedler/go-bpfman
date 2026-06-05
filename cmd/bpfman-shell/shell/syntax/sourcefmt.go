package syntax

import (
	"fmt"
	"strings"
)

// FormatExprSource renders expr in the shell's compact source-like form.
// Used by diagnostics and traces that need the original expression shape
// without reimplementing the expression formatter.
func FormatExprSource(expr Expr) string {
	return dumpExprSource(expr)
}

// FormatAssertClauseSource renders one assertion clause in the shell's
// compact source-like form.
func FormatAssertClauseSource(clause AssertClause) string {
	return dumpAssertClauseSource(clause)
}

func dumpExprSource(e Expr) string {
	if e == nil {
		return "nil"
	}
	var b strings.Builder
	writeExprSource(&b, e)
	return b.String()
}

func dumpAssertClauseSource(c AssertClause) string {
	if c == nil {
		return "<nil-assert-clause>"
	}
	var b strings.Builder
	writeAssertClauseSource(&b, c)
	return b.String()
}

func writeExprSource(b *strings.Builder, e Expr) {
	switch v := e.(type) {
	case *LiteralExpr:
		if v.Quoted {
			fmt.Fprintf(b, "%q", v.Text)
			return
		}
		b.WriteString(v.Text)
	case *VarRefExpr:
		b.WriteByte('$')
		b.WriteString(v.Name)
		if v.Path != "" {
			b.WriteByte('.')
			b.WriteString(v.Path)
		}
	case *AdapterExpr:
		b.WriteString(v.Adapter)
		b.WriteByte(':')
		b.WriteByte('$')
		b.WriteString(v.Name)
		if v.Path != "" {
			b.WriteByte('.')
			b.WriteString(v.Path)
		}
	case *InterpStringExpr:
		b.WriteByte('"')
		for _, seg := range v.Segments {
			if seg.Expr == nil {
				b.WriteString(seg.Literal)
				continue
			}
			b.WriteString("${")
			switch sub := seg.Expr.(type) {
			case *VarRefExpr:
				b.WriteString(sub.Name)
				if sub.Path != "" {
					b.WriteByte('.')
					b.WriteString(sub.Path)
				}
			case *AdapterExpr:
				b.WriteString(sub.Adapter)
				b.WriteByte(':')
				b.WriteString(sub.Name)
				if sub.Path != "" {
					b.WriteByte('.')
					b.WriteString(sub.Path)
				}
			default:
				writeExprSource(b, seg.Expr)
			}
			b.WriteByte('}')
		}
		b.WriteByte('"')
	case *BinaryExpr:
		writeExprAtomSource(b, v.Left)
		b.WriteByte(' ')
		b.WriteString(v.Op)
		b.WriteByte(' ')
		writeExprAtomSource(b, v.Right)
	case *UnaryExpr:
		b.WriteString(v.Pred)
		b.WriteByte(' ')
		writeExprAtomSource(b, v.Operand)
	case *ThreadExpr:
		writeExprAtomSource(b, v.LHS)
		b.WriteString(" |>")
		for _, a := range v.Args {
			b.WriteByte(' ')
			writeExprSource(b, a)
		}
	case *LogicalExpr:
		writeExprAtomSource(b, v.Left)
		b.WriteByte(' ')
		b.WriteString(v.Op)
		b.WriteByte(' ')
		writeExprAtomSource(b, v.Right)
	case *NotExpr:
		b.WriteString("not ")
		writeExprAtomSource(b, v.Operand)
	case *NegateExpr:
		b.WriteByte('-')
		writeExprAtomSource(b, v.Operand)
	case *PureCallExpr:
		b.WriteString(v.Name)
		for _, a := range v.Args {
			b.WriteByte(' ')
			writeExprSource(b, a)
		}
	case *MatchesExpr:
		writeExprAtomSource(b, v.Target)
		b.WriteByte(' ')
		writeMatchesBlockSource(b, v.Block)
	case *ListExpr:
		b.WriteByte('[')
		for i, elem := range v.Elems {
			if i > 0 {
				b.WriteByte(' ')
			}
			writeExprSource(b, elem)
		}
		b.WriteByte(']')
	case *RecordExpr:
		b.WriteString("record {")
		for i, field := range v.Fields {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(field.Name)
			b.WriteString(": ")
			writeExprSource(b, field.Expr)
		}
		b.WriteByte('}')
	default:
		t := fmt.Sprintf("%T", e)
		if i := strings.LastIndex(t, "."); i >= 0 {
			t = t[i+1:]
		}
		fmt.Fprintf(b, "<%s>", t)
	}
}

func writeAssertClauseSource(b *strings.Builder, c AssertClause) {
	switch v := c.(type) {
	case *AssertExprClause:
		writeExprSource(b, v.Expr)
	case *AssertCommandClause:
		if v.Negate {
			b.WriteString("not ")
		}
		b.WriteString(v.Head)
		for _, a := range v.Args {
			b.WriteByte(' ')
			writeExprSource(b, a)
		}
	default:
		t := fmt.Sprintf("%T", c)
		if i := strings.LastIndex(t, "."); i >= 0 {
			t = t[i+1:]
		}
		fmt.Fprintf(b, "<%s>", t)
	}
}

func writeExprAtomSource(b *strings.Builder, e Expr) {
	switch e.(type) {
	case *BinaryExpr, *LogicalExpr, *ThreadExpr, *UnaryExpr, *NotExpr, *NegateExpr, *MatchesExpr:
		b.WriteByte('(')
		writeExprSource(b, e)
		b.WriteByte(')')
	default:
		writeExprSource(b, e)
	}
}

func writeMatchesBlockSource(b *strings.Builder, m *MatchesBlockExpr) {
	b.WriteString("matches")
	if m.Exhaustive {
		b.WriteString(" exhaustive")
	}
	b.WriteString(" {")
	for i, ent := range m.Entries {
		// The matches parser separates entries by newlines and
		// explicitly rejects commas; emitting commas here would
		// produce a string that does not round-trip through
		// Parse. Use newlines so the printed form is a valid
		// matches body, falling back to a single space before
		// the first entry so an empty block still renders as
		// "matches { }" rather than "matches {\n}".
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteByte(' ')
		b.WriteString(ent.Path)
		b.WriteString(": ")
		switch {
		case ent.Predicate != "":
			b.WriteString(ent.Predicate)
		case ent.SubBlock != nil:
			writeMatchesBlockSource(b, ent.SubBlock)
		default:
			writeExprSource(b, ent.Pattern)
		}
	}
	if len(m.Entries) > 0 {
		b.WriteByte(' ')
	}
	b.WriteByte('}')
}
