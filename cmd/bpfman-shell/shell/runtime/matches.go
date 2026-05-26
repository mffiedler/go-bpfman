package runtime

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"

func evalMatchesBlock(e *syntax.MatchesBlockExpr, env *Env) (resolvedMatchesBlock, error) {
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
			sub, err := evalMatchesBlock(entry.SubBlock, env)
			if err != nil {
				return resolvedMatchesBlock{}, err
			}
			ent.SubBlock = &sub
		case entry.Predicate != "":
			// nothing to evaluate; the predicate carries the
			// assertion intent.
		default:
			v, err := EvalExpr(entry.Pattern, env)
			if err != nil {
				return resolvedMatchesBlock{}, syntax.SpanErrorf(entry.Span, "matches entry %q: %v", entry.Path, err)
			}
			ent.Value = v
		}
		out.Entries = append(out.Entries, ent)
	}
	return out, nil
}
