// Tab-completion for the REPL, lifted out of repl.go so the
// main loop can stay focused on parsing and dispatch.  Everything
// here is pure lookups against the live manager and the shell
// session; no completion code mutates state.  Tests live in
// repl_test.go under TestReplComplete* and TestReplCompleteVarPath*.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/shell"
)

// replCommandNames lists the top-level command tokens for completion.
// Domain commands live behind the "bpfman" prefix; shell-language
// commands are bare.
var replCommandNames = []string{"alias", "aliases", "assert", "bpfman", "exec", "file", "help", "jq", "let", "print", "require", "source", "unalias", "unset", "vars", "version"}

// replAssertVerbs lists the valid assertion verbs for completion.
var replAssertVerbs = []string{"contains", "fail", "false", "nil", "not", "not-empty", "ok", "path", "true"}

// replSubcommands maps a top-level token to its valid subcommands for completion.
var replSubcommands = map[string][]string{
	"assert":  replAssertVerbs,
	"bpfman":  {"dispatcher", "doctor", "gc", "link", "program", "programs", "show"},
	"exec":    {"status"},
	"file":    {"temp"},
	"require": replAssertVerbs,
}

// bpfmanSubcommands maps a bpfman domain token to its valid
// subcommands for completion.
var bpfmanSubcommands = map[string][]string{
	"dispatcher": {"delete", "get", "list"},
	"doctor":     {"checkup", "explain"},
	"link":       {"attach", "delete", "detach", "get", "list"},
	"program":    {"delete", "get", "list", "load", "unload"},
	"programs":   {"list"},
	"show":       {"program"},
}

// replAttachTypes lists the valid attach types for "link attach <type>".
var replAttachTypes = []string{"fentry", "fexit", "kprobe", "tc", "tcx", "tracepoint", "uprobe", "xdp"}

// showProgramViews lists the valid sub-view names for "show program <id>".
var showProgramViews = []string{"links", "maps", "paths"}

// replCompleter returns a CompleteFunc that has access to the manager
// and session for dynamic completions such as program IDs and
// variable names.
func replCompleter(ctx context.Context, mgr *manager.Manager, session *shell.Session) CompleteFunc {
	return func(line string, pos int) (replace int, candidates []string) {
		return replComplete(ctx, mgr, session, line, pos)
	}
}

func replComplete(ctx context.Context, mgr *manager.Manager, session *shell.Session, line string, pos int) (replace int, candidates []string) {
	return replCompleteIn(ctx, mgr, session, "", line, pos)
}

// replCompleteIn is replComplete with an explicit base directory
// for filesystem completions. When baseDir is empty, relative
// completions resolve against the process working directory; when
// set, they resolve under baseDir. Tests use this entry point to
// avoid mutating the process cwd via os.Chdir, which is unsafe
// under t.Parallel().
func replCompleteIn(ctx context.Context, mgr *manager.Manager, session *shell.Session, baseDir, line string, pos int) (replace int, candidates []string) {
	head := line[:pos]

	tokens := strings.Fields(head)
	trailingSpace := len(head) > 0 && head[len(head)-1] == ' '

	// Detect --path / -p flag completion: if the previous token is
	// "--path" or "-p", offer filesystem completions for the
	// current partial token.
	isPathFlag := func(tok string) bool {
		return tok == "--path" || tok == "-p"
	}
	if len(tokens) >= 2 && trailingSpace && isPathFlag(tokens[len(tokens)-1]) {
		// Cursor is right after "--path " or "-p ", complete filesystem paths.
		candidates = replFileCompletions(baseDir, "")
		return
	}
	if len(tokens) >= 2 && !trailingSpace {
		prevIdx := len(tokens) - 2
		if isPathFlag(tokens[prevIdx]) {
			prefix := tokens[len(tokens)-1]
			candidates = replFileCompletions(baseDir, prefix)
			replace = len(prefix)
			return
		}
	}

	// When the first token is "bpfman", delegate to domain
	// completion with the prefix stripped.
	if len(tokens) >= 1 && tokens[0] == "bpfman" {
		return replCompleteBpfman(ctx, mgr, session, tokens[1:], trailingSpace)
	}

	switch {
	case len(tokens) == 0 || (len(tokens) == 1 && !trailingSpace):
		// Completing the first token.
		prefix := ""
		if len(tokens) == 1 {
			prefix = tokens[0]
		}
		// A '$'-led first token is an expression statement
		// starting with a variable reference: offer variable
		// path completions so "$prog.<tab>" walks through an
		// object's fields.  Bare-word first tokens stay on the
		// command-name path — a literal "prog" is a command
		// invocation, not a variable lookup.
		if strings.HasPrefix(prefix, "$") {
			candidates, replace = replCompleteVarPath(session, prefix, true)
			return
		}
		for _, cmd := range replCommandNames {
			if strings.HasPrefix(cmd, prefix) {
				candidates = append(candidates, cmd+" ")
			}
		}
		replace = len(prefix)
	case (len(tokens) == 1 && trailingSpace) || (len(tokens) == 2 && !trailingSpace):
		// "source" takes a file path as its argument.
		if tokens[0] == "source" {
			if len(tokens) == 2 {
				candidates = replFileCompletions(baseDir, tokens[1])
				replace = len(tokens[1])
			} else {
				candidates = replFileCompletions(baseDir, "")
			}
			return
		}
		// "print" takes any expression — including a variable
		// reference — as its argument.  Bare-word arguments
		// are literal strings at runtime ("print foo" prints
		// "foo", not the variable foo), so completion only
		// offers variable paths when the prefix is
		// sigil-led.  An empty or "$"-only prefix lists every
		// variable; a partial sigil-led prefix narrows by
		// name; a bare partial token defers to the
		// command-path fallback (no variable completions, and
		// no shadowing of the literal-string semantics).
		if tokens[0] == "print" {
			prefix := ""
			if len(tokens) == 2 {
				prefix = tokens[1]
			}
			if prefix == "" || strings.HasPrefix(prefix, "$") {
				candidates, replace = replCompleteVarPath(session, prefix, true)
				return
			}
			return
		}
		// "unset" takes bare variable names.
		if tokens[0] == "unset" {
			prefix := ""
			if len(tokens) == 2 {
				prefix = tokens[1]
			}
			candidates, replace = replCompleteVarNames(session, prefix)
			return
		}
		// Completing the second token (subcommand).
		subs := replSubcommands[tokens[0]]
		prefix := ""
		if len(tokens) == 2 {
			prefix = tokens[1]
		}
		for _, sub := range subs {
			if strings.HasPrefix(sub, prefix) {
				candidates = append(candidates, sub+" ")
			}
		}
		replace = len(prefix)
	default:
		// Third token onwards: context-sensitive completion.
		candidates, replace = replCompleteArgs(ctx, mgr, session, tokens, trailingSpace)
	}

	return
}

// replCompleteBpfman handles completion for tokens after the leading
// "bpfman" prefix. The args slice has the prefix already stripped.
func replCompleteBpfman(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (replace int, candidates []string) {
	// "bpfman " or "bpfman dis..." -- complete domain command names.
	if len(args) == 0 || (len(args) == 1 && !trailingSpace) {
		prefix := ""
		if len(args) == 1 {
			prefix = args[0]
		}
		for _, sub := range replSubcommands["bpfman"] {
			if strings.HasPrefix(sub, prefix) {
				candidates = append(candidates, sub+" ")
			}
		}
		replace = len(prefix)
		return
	}

	// "bpfman program " -- complete second-level domain subcommand.
	if (len(args) == 1 && trailingSpace) || (len(args) == 2 && !trailingSpace) {
		subs := bpfmanSubcommands[args[0]]
		prefix := ""
		if len(args) == 2 {
			prefix = args[1]
		}
		for _, sub := range subs {
			if strings.HasPrefix(sub, prefix) {
				candidates = append(candidates, sub+" ")
			}
		}
		replace = len(prefix)
		return
	}

	// Third token onwards: delegate to the same arg completer,
	// using the domain tokens (without the "bpfman" prefix).
	candidates, replace = replCompleteArgs(ctx, mgr, session, args, trailingSpace)
	return
}

// replCompleteArgs handles completion for the third token onwards,
// dispatching based on the command prefix.
func replCompleteArgs(ctx context.Context, mgr *manager.Manager, session *shell.Session, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	if len(tokens) < 2 {
		return
	}
	if tokens[0] == "unset" {
		prefix := ""
		if !trailingSpace {
			prefix = tokens[len(tokens)-1]
		}
		return replCompleteVarNames(session, prefix)
	}

	// program subcommands
	if tokens[0] == "program" {
		switch tokens[1] {
		case "delete":
			return replCompleteProgramDelete(ctx, mgr, session, tokens[2:], trailingSpace)
		case "get":
			return replCompleteProgramIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "unload":
			return replCompleteProgramIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "load":
			// "program load file" or "program load image" -- third-level subcommand
			if len(tokens) == 2 && trailingSpace {
				return []string{"file ", "image "}, 0
			}
			if len(tokens) == 3 && !trailingSpace {
				prefix := tokens[2]
				for _, sub := range []string{"file", "image"} {
					if strings.HasPrefix(sub, prefix) {
						candidates = append(candidates, sub+" ")
					}
				}
				return candidates, len(prefix)
			}
			return
		}
	}

	if tokens[0] == "show" && tokens[1] == "program" {
		return replCompleteShowProgram(ctx, mgr, session, tokens[2:], trailingSpace)
	}

	// link subcommands
	if tokens[0] == "link" {
		switch tokens[1] {
		case "attach":
			return replCompleteLinkAttach(ctx, mgr, session, tokens[2:], trailingSpace)
		case "detach":
			return replCompleteLinkIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "get":
			return replCompleteLinkIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "delete":
			return replCompleteLinkIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		}
	}

	return
}

// replCompleteLinkAttach handles completion for "link attach ...".
// First arg is attach type, remaining args get program ID completion.
func replCompleteLinkAttach(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	switch {
	case len(args) == 0 && trailingSpace:
		// "link attach " -- complete attach types
		for _, t := range replAttachTypes {
			candidates = append(candidates, t+" ")
		}
		return
	case len(args) == 1 && !trailingSpace:
		// "link attach xd" -- partial attach type
		prefix := args[0]
		for _, t := range replAttachTypes {
			if strings.HasPrefix(t, prefix) {
				candidates = append(candidates, t+" ")
			}
		}
		return candidates, len(prefix)
	default:
		// After the attach type, offer program ID completion for
		// tokens that look like they could be a program ID argument.
		// We only complete the last token if it starts with $ or is numeric.
		if !trailingSpace && len(args) > 1 {
			last := args[len(args)-1]
			if strings.HasPrefix(last, "$") {
				return replCompleteVarPath(session, last, true)
			}
		}
		if trailingSpace {
			// Offer program IDs and $variables.
			return replCompleteProgramIDs(ctx, mgr, session, nil, true)
		}
		return
	}
}

// replCompleteShowProgram handles completion for "show program ..."
// arguments. The first argument is a program ID; the second is a
// sub-view name.
func replCompleteShowProgram(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	switch {
	case len(args) == 0 && trailingSpace:
		// "show program " -- complete program IDs
		return replCompleteProgramIDs(ctx, mgr, session, args, trailingSpace)
	case len(args) == 1 && !trailingSpace:
		// "show program 12" -- partial program ID
		return replCompleteProgramIDs(ctx, mgr, session, args, trailingSpace)
	case len(args) == 1 && trailingSpace:
		// "show program 123 " -- complete sub-view
		for _, v := range showProgramViews {
			candidates = append(candidates, v+" ")
		}
		return
	case len(args) == 2 && !trailingSpace:
		// "show program 123 li" -- partial sub-view
		prefix := args[1]
		for _, v := range showProgramViews {
			if strings.HasPrefix(v, prefix) {
				candidates = append(candidates, v+" ")
			}
		}
		replace = len(prefix)
		return
	}
	return
}

// replCompleteProgramDelete handles completion for "program delete",
// offering --all in addition to program IDs.
func replCompleteProgramDelete(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	candidates, replace = replCompleteProgramIDs(ctx, mgr, session, args, trailingSpace)

	// Determine the current prefix.
	prefix := ""
	if !trailingSpace && len(args) > 0 {
		prefix = args[len(args)-1]
	}

	// Offer --all when it matches the prefix and hasn't been specified.
	if strings.HasPrefix("--all", prefix) {
		already := false
		for _, a := range args {
			if a == "--all" {
				already = true
				break
			}
		}
		if !already {
			candidates = append(candidates, "--all ")
		}
	}

	return
}

// replCompleteProgramIDs offers program ID completions, excluding IDs
// that have already been specified on the command line. When the
// prefix starts with '$', completion is delegated to
// replCompleteVarPath for dotted path support. Otherwise, numeric IDs
// and top-level $variable names are offered.
func replCompleteProgramIDs(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	// Collect IDs already on the line so we don't offer them again.
	specified := make(map[string]struct{}, len(args))
	for _, a := range args {
		specified[a] = struct{}{}
	}

	prefix := ""
	if !trailingSpace && len(args) > 0 {
		// The last token is a partial ID being typed.
		prefix = args[len(args)-1]
		delete(specified, prefix)
	}

	// When the prefix starts with '$', delegate to path completion.
	if strings.HasPrefix(prefix, "$") {
		candidates, replace = replCompleteVarPath(session, prefix, true)
		return
	}

	if mgr != nil {
		result, err := mgr.ListPrograms(ctx)
		if err == nil {
			for _, prog := range result.Programs {
				id := fmt.Sprintf("%d", prog.Record.ProgramID)
				if _, already := specified[id]; already {
					continue
				}
				if strings.HasPrefix(id, prefix) {
					candidates = append(candidates, id+" ")
				}
			}
		}
	}

	// Offer $variable completions from the session when no prefix
	// or a non-$ prefix is being typed.
	if session != nil {
		for _, name := range session.Names() {
			candidate := "$" + name
			if _, already := specified[candidate]; already {
				continue
			}
			if !strings.HasPrefix(candidate, prefix) {
				continue
			}
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			if v.IsStructured() {
				// Only offer if it has .record.program_id
				if _, err := v.LookupValue(name, "record.program_id"); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			} else if v.IsScalar() {
				s, err := v.Scalar()
				if err != nil {
					continue
				}
				if _, err := ParseProgramID(s); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			}
		}
	}

	replace = len(prefix)
	return
}

// replCompleteLinkIDs offers link ID completions, analogous to
// replCompleteProgramIDs.
func replCompleteLinkIDs(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	specified := make(map[string]struct{}, len(args))
	for _, a := range args {
		specified[a] = struct{}{}
	}

	prefix := ""
	if !trailingSpace && len(args) > 0 {
		prefix = args[len(args)-1]
		delete(specified, prefix)
	}

	if strings.HasPrefix(prefix, "$") {
		candidates, replace = replCompleteVarPath(session, prefix, true)
		return
	}

	if mgr != nil {
		links, err := mgr.ListLinks(ctx)
		if err == nil {
			for _, l := range links {
				id := fmt.Sprintf("%d", l.ID)
				if _, already := specified[id]; already {
					continue
				}
				if strings.HasPrefix(id, prefix) {
					candidates = append(candidates, id+" ")
				}
			}
		}
	}

	if session != nil {
		for _, name := range session.Names() {
			candidate := "$" + name
			if _, already := specified[candidate]; already {
				continue
			}
			if !strings.HasPrefix(candidate, prefix) {
				continue
			}
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			if v.IsStructured() {
				if _, err := v.LookupValue(name, "record.id"); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			} else if v.IsScalar() {
				s, err := v.Scalar()
				if err != nil {
					continue
				}
				if _, err := ParseLinkID(s); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			}
		}
	}

	replace = len(prefix)
	return
}

// replCompleteVarPath completes dotted variable paths. The token is
// the partial text (e.g. "$prog.rec" or "prog.record."). When sigil
// is true, variable names carry a '$' prefix (program ID contexts);
// when false they are bare (print context). Returns candidates and the
// number of characters to replace (the full token length).
func replCompleteVarPath(session *shell.Session, token string, sigil bool) (candidates []string, replace int) {
	if session == nil {
		return
	}

	replace = len(token)

	// Strip the '$' prefix when present.
	stripped := token
	if sigil && strings.HasPrefix(stripped, "$") {
		stripped = stripped[1:]
	}

	// Empty remainder: list all variable names.
	if stripped == "" {
		for _, name := range session.Names() {
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			candidate := name
			if sigil {
				candidate = "$" + name
			}
			// Append trailing character based on value type.
			candidate += varPathSuffix(v)
			candidates = append(candidates, candidate)
		}
		return
	}

	// Find the split point: first '.' or '['.
	sepIdx := strings.IndexAny(stripped, ".[")

	// No separator and token does not end with '.' -- partial variable name.
	if sepIdx < 0 {
		for _, name := range session.Names() {
			if !strings.HasPrefix(name, stripped) {
				continue
			}
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			candidate := name
			if sigil {
				candidate = "$" + name
			}
			candidates = append(candidates, candidate+varPathSuffix(v))
			// Discovery: when the token exactly matches a
			// structured variable name, also enumerate its
			// immediate children so a single "$prog<tab>"
			// surfaces drillable fields without forcing the
			// user through a trailing-separator candidate.
			// Each child candidate is itself a valid
			// expression, so Enter works at any level.
			if name == stripped && v.IsStructured() {
				candidates = append(candidates, enumerateChildren(candidate, name, v)...)
			}
		}
		return
	}

	varName := stripped[:sepIdx]
	v, ok := session.Get(varName)
	if !ok {
		return
	}

	pathPart := stripped[sepIdx:]

	// Determine the resolved prefix (complete segments), the
	// tail (partial last segment after the final '.' or '['),
	// and whether the input is a fully-resolved path with no
	// partial tail at all.
	var resolvedPath, tail string
	atResolved := false
	if strings.HasSuffix(pathPart, ".") {
		// e.g. "record." -- walk to "record", enumerate keys
		resolvedPath = strings.TrimPrefix(pathPart, ".")
		resolvedPath = strings.TrimSuffix(resolvedPath, ".")
		tail = ""
	} else if strings.HasSuffix(pathPart, "[") {
		// e.g. "maps[" -- walk to "maps", enumerate indices
		resolvedPath = strings.TrimPrefix(pathPart, ".")
		resolvedPath = strings.TrimSuffix(resolvedPath, "[")
		tail = "["
	} else if strings.HasSuffix(pathPart, "]") {
		// e.g. "maps[0]" -- fully resolved to an array
		// element.  No partial tail; offer the resolved path
		// itself, plus child-discovery if the value is
		// structured.
		resolvedPath = strings.TrimPrefix(pathPart, ".")
		tail = ""
		atResolved = true
	} else {
		// e.g. "record.prog" -- walk to "record", match "prog"
		lastDot := strings.LastIndex(pathPart, ".")
		lastBracket := strings.LastIndex(pathPart, "[")
		if lastDot >= lastBracket {
			resolvedPath = strings.TrimPrefix(pathPart[:lastDot], ".")
			tail = pathPart[lastDot+1:]
		} else {
			// Partial array index like "maps[1" -- not useful to complete
			return
		}
	}

	// Walk to the resolved prefix.
	target, err := v.LookupValue(varName, resolvedPath)
	if err != nil {
		return
	}

	// Build the candidate prefix: everything before the tail.
	var candidatePrefix string
	if sigil {
		candidatePrefix = "$"
	}
	candidatePrefix += varName
	if resolvedPath != "" {
		candidatePrefix += "." + resolvedPath
	}

	if atResolved {
		// Fully-resolved bracketed path (e.g. "maps[0]").
		// Emit the resolved candidate itself, plus its
		// children when structured — mirrors the discovery
		// rule applied at the variable-name and partial-field
		// levels.
		candidates = append(candidates, candidatePrefix+varPathSuffix(target))
		if target.IsStructured() {
			candidates = append(candidates, enumerateChildren(candidatePrefix, varName, target)...)
		}
		return
	}

	keys := target.Keys()
	if keys == nil {
		return
	}

	if tail == "[" {
		// Array index completion.
		for _, key := range keys {
			if !strings.HasPrefix(key, "[") {
				continue
			}
			// Walk to the element to determine its trailing character.
			elemPath := resolvedPath
			if elemPath != "" {
				elemPath += key
			} else {
				elemPath = key
			}
			elem, err := v.LookupValue(varName, elemPath)
			if err != nil {
				continue
			}
			candidate := candidatePrefix + key + varPathSuffix(elem)
			candidates = append(candidates, candidate)
		}
		return
	}

	// Child completion: match keys against tail prefix.  The
	// separator between candidatePrefix and key depends on the
	// key shape — map field names take a leading '.', array
	// indices arrive as "[n]" and glue directly on.  target is
	// usually a map here (so keys are field names), but a
	// trailing-dot path on an array variable (e.g. "maps.")
	// reaches this loop with array-shaped keys; without the
	// per-key separator choice the candidate would be
	// "maps.[0]", which is not a valid variable reference.
	for _, key := range keys {
		if !strings.HasPrefix(key, tail) {
			continue
		}
		isIndex := strings.HasPrefix(key, "[")
		sep := "."
		if isIndex {
			sep = ""
		}
		// Walk to the field to determine its trailing character.
		fieldPath := resolvedPath
		if fieldPath != "" {
			fieldPath += sep + key
		} else {
			fieldPath = key
		}
		child, err := v.LookupValue(varName, fieldPath)
		if err != nil {
			continue
		}
		base := candidatePrefix + sep + key
		candidates = append(candidates, base+varPathSuffix(child))
		// Discovery: when the tail exactly matches a field
		// whose value is structured, also enumerate its
		// children so a second tab on "$prog.record<tab>"
		// surfaces "$prog.record.program_id" etc.
		if key == tail && child.IsStructured() {
			candidates = append(candidates, enumerateChildren(base, varName, child)...)
		}
	}
	return
}

// enumerateChildren returns candidate strings for each direct
// child of v, each formed as `base + key + varPathSuffix(child)`
// with the appropriate separator: "." for map fields and "" for
// array indices (since Keys() already returns bracketed tokens
// like "[0]").  Errors during per-key lookup are skipped — the
// completer never bails out just because one child is
// unreachable.  varName seeds LookupValue's error messages; it
// does not affect the candidate text.
func enumerateChildren(base, varName string, v shell.Value) []string {
	var out []string
	for _, key := range v.Keys() {
		// Determine the path segment used both for lookup
		// (relative to v) and for the separator in the
		// emitted candidate.  Map keys are bare idents; array
		// keys arrive pre-bracketed as "[n]".
		isIndex := strings.HasPrefix(key, "[")
		lookupPath := key
		if !isIndex {
			lookupPath = key
		}
		child, err := v.LookupValue(varName, lookupPath)
		if err != nil {
			continue
		}
		sep := "."
		if isIndex {
			sep = ""
		}
		out = append(out, base+sep+key+varPathSuffix(child))
	}
	return out
}

// replCompleteVarNames offers bare variable name completions with a
// trailing space. Used by commands like unset that take whole variable
// names rather than dotted paths.
func replCompleteVarNames(session *shell.Session, prefix string) (candidates []string, replace int) {
	if session == nil {
		return
	}
	replace = len(prefix)
	for _, name := range session.Names() {
		if strings.HasPrefix(name, prefix) {
			candidates = append(candidates, name+" ")
		}
	}
	return
}

// varPathSuffix returns the trailing character for a completion
// candidate based on the value type.  Scalars get " " (terminal
// leaf — commit and move on).  Maps and arrays get "" so the
// completed token stays a valid expression the user can press
// Enter on; drilling one more level requires typing "." or "["
// explicitly.  A trailing "." or "[" would be a parse error on
// its own ("expected identifier after '.'"), so appending it
// forces a backspace to stop at any given level — the friction
// this no-suffix rule avoids.
func varPathSuffix(v shell.Value) string {
	switch v.Raw().(type) {
	case map[string]any, []any:
		return ""
	default:
		return " "
	}
}

// replFileCompletions returns filesystem path completions for the
// given prefix. Directories get a trailing slash. When the prefix
// starts with "./" the dot-slash is preserved in completions because
// filepath.Glob normalises it away. baseDir, when non-empty,
// resolves relative prefixes under that directory rather than the
// process cwd; absolute prefixes ignore it.
func replFileCompletions(baseDir, prefix string) []string {
	// When no prefix is given, list the current directory with an
	// explicit "./" so completions read as relative paths.
	dotSlash := false
	globPrefix := prefix
	if globPrefix == "" {
		globPrefix = "./"
		dotSlash = true
	} else if strings.HasPrefix(globPrefix, "./") {
		dotSlash = true
	}

	rebase := baseDir != "" && !filepath.IsAbs(globPrefix)
	pattern := globPrefix + "*"
	if rebase {
		// Strip a leading "./" so it doesn't get folded away by
		// filepath.Join, then anchor under baseDir. The user's
		// dot-slash convention is restored on the way back out.
		rel := strings.TrimPrefix(globPrefix, "./")
		pattern = baseDir + string(filepath.Separator) + rel + "*"
	}

	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil
	}
	var completions []string
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		out := m
		if rebase {
			out = strings.TrimPrefix(m, baseDir+string(filepath.Separator))
			if dotSlash {
				out = "./" + out
			}
		} else if dotSlash && !strings.HasPrefix(out, "./") {
			// filepath.Glob strips the "./" prefix; restore it
			// when the user typed one.
			out = "./" + out
		}
		if info.IsDir() {
			completions = append(completions, out+"/")
		} else {
			completions = append(completions, out)
		}
	}
	return completions
}
