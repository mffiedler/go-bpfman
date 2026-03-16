package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/coherency"
	"github.com/frobware/go-bpfman/shell"
	"github.com/frobware/go-bpfman/version"
)

// ReplCmd starts an interactive shell for inspecting BPF state.
// When --file is given, commands are read from the named file. When
// stdin is not a terminal, commands are read from stdin. Otherwise an
// interactive readline prompt is started.
type ReplCmd struct {
	File string `name:"file" short:"f" help:"Read commands from a file (use '-' for stdin)."`
}

// replCommandNames lists the top-level command tokens for completion.
var replCommandNames = []string{"assert", "dispatcher", "doctor", "dump", "exec", "file", "gc", "help", "json", "let", "link", "list", "load", "program", "programs", "require", "set", "show", "source", "unset", "vars", "version"}

// replSubcommands maps a top-level token to its valid subcommands for completion.
// replAssertVerbs lists the valid assertion verbs for completion.
var replAssertVerbs = []string{"contains", "equal", "fail", "false", "ge", "gt", "le", "lt", "ne", "nil", "not", "not-empty", "ok", "path", "true"}

var replSubcommands = map[string][]string{
	"assert":     replAssertVerbs,
	"dispatcher": {"delete", "get", "list"},
	"doctor":     {"checkup", "explain"},
	"exec":       {"status"},
	"file":       {"temp"},
	"json":       {"parse"},
	"link":       {"attach", "delete", "detach", "get", "list"},
	"list":       {"programs"},
	"load":       {"file", "image"},
	"program":    {"delete", "get", "list", "load", "unload"},
	"programs":   {"list"},
	"require":    replAssertVerbs,
	"show":       {"program"},
}

// replAttachTypes lists the valid attach types for "link attach <type>".
var replAttachTypes = []string{"fentry", "fexit", "kprobe", "tc", "tcx", "tracepoint", "uprobe", "xdp"}

// errRequireFailed is the sentinel error used to halt script execution
// when a require assertion fails.
var errRequireFailed = errors.New("require failed")

// errScriptError is the sentinel error used to halt script execution
// when a command error occurs in file mode. The error message has
// already been printed with a source location prefix.
var errScriptError = errors.New("script error")

// assertResult holds the outcome of evaluating an assertion verb.
type assertResult struct {
	pass    bool
	message string
}

// Run starts the read-eval-print loop. A single manager is held open
// for the session lifetime to avoid repeated store open/close.
func (c *ReplCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManagerWithPuller(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	session := shell.NewSession()

	lr, err := c.newReader(ctx, mgr, session)
	if err != nil {
		return err
	}
	defer lr.Close()

	file := c.File
	if file == "-" || (file == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		file = "<stdin>"
	}
	loopErr := replLoop(ctx, cli, mgr, lr, session, file)

	if errors.Is(loopErr, errRequireFailed) || errors.Is(loopErr, errScriptError) {
		return ErrSilent
	}
	if loopErr != nil {
		return loopErr
	}

	if n := session.AssertFailures(); n > 0 {
		_ = cli.PrintErrf("%d assertion(s) failed\n", n)
		return fmt.Errorf("%d assertion(s) failed", n)
	}

	return nil
}

// newReader selects the appropriate LineReader: file, pipe, or
// interactive readline.
func (c *ReplCmd) newReader(ctx context.Context, mgr *manager.Manager, session *shell.Session) (LineReader, error) {
	if c.File != "" {
		return openScriptReader(c.File)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return NewScannerReader(os.Stdin, nil), nil
	}
	historyPath, err := replHistoryPath()
	if err != nil {
		return nil, fmt.Errorf("history path: %w", err)
	}
	return NewLineReader("bpfman> ", historyPath, replCompleter(ctx, mgr, session))
}

// openScriptReader opens a file for reading commands. Use "-" to
// read from stdin.
func openScriptReader(path string) (LineReader, error) {
	if path == "-" {
		return NewScannerReader(os.Stdin, nil), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open script: %w", err)
	}
	return NewScannerReader(f, f), nil
}

// sourceLoc identifies a position in a script file. The zero value
// means "no location" and formats as the empty string, so interactive
// and piped-stdin modes are unaffected.
type sourceLoc struct {
	file string
	line int
}

func (l sourceLoc) String() string {
	if l.file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d: ", l.file, l.line)
}

// replLoop reads lines from lr and dispatches them until EOF or
// interrupt. Blank lines and comments are handled by
// shell.Tokenise. Variable assignment and expansion use the
// shell.Session. When file is non-empty, error messages include a
// file:line: prefix for compiler-style diagnostics.
func replLoop(ctx context.Context, cli *CLI, mgr *manager.Manager, lr LineReader, session *shell.Session, file string) error {
	var lineNo int
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				return nil
			}
			return err
		}
		lineNo++

		var loc sourceLoc
		if file != "" {
			loc = sourceLoc{file: file, line: lineNo}
		}

		if err := replEval(ctx, cli, mgr, session, input, loc); err != nil {
			return err
		}
	}
}

// shellCommands is the set of commands that are shell-language or
// session concerns rather than bpfman domain commands. These are
// handled directly by replEval and never reach the domain command
// dispatcher.
var shellCommands = map[string]bool{
	"assert":  true,
	"exec":    true,
	"file":    true,
	"json":    true,
	"require": true,
	"dump":    true,
	"help":    true,
	"source":  true,
	"unset":   true,
	"vars":    true,
	"version": true,
}

// replShellCmd handles shell-language and session commands. It returns
// (true, value, err) if the command was handled, where value is
// non-nil for commands that produce an assignable result (e.g. exec).
// Returns (false, Value{}, nil) if the command is not a shell command
// and should be dispatched to the domain layer.
func replShellCmd(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, loc sourceLoc) (bool, shell.Value, error) {
	if len(args) == 0 {
		return false, shell.Value{}, nil
	}
	cmd := argText(args[0])
	if !shellCommands[cmd] {
		return false, shell.Value{}, nil
	}

	switch cmd {
	case "assert":
		return true, shell.Value{}, replAssertRequire(ctx, cli, mgr, session, args[1:], false, loc)
	case "exec":
		val, err := replExec(ctx, cli, args[1:])
		return true, val, err
	case "file":
		val, err := replFile(cli, session, args[1:])
		return true, val, err
	case "json":
		val, err := replJSON(cli, args[1:])
		return true, val, err
	case "require":
		return true, shell.Value{}, replAssertRequire(ctx, cli, mgr, session, args[1:], true, loc)
	case "dump":
		return true, shell.Value{}, replDump(cli, session, argTexts(args[1:]))
	case "help":
		return true, shell.Value{}, replHelp(cli)
	case "source":
		return true, shell.Value{}, replSource(ctx, cli, mgr, session, argTexts(args[1:]))
	case "unset":
		return true, shell.Value{}, replUnset(cli, session, argTexts(args[1:]))
	case "vars":
		return true, shell.Value{}, replVars(cli, session)
	case "version":
		return true, shell.Value{}, replVersion(cli)
	default:
		return false, shell.Value{}, nil
	}
}

// replEval processes a single input line: tokenise, parse, expand
// variables, dispatch, and optionally bind the result. Shell-language
// commands (assert, require, dump, help, source, unset, vars, version)
// are handled directly. Domain commands flow to replDispatch. In
// interactive mode (loc has no file), non-fatal errors are printed and
// return nil so the session continues. In script mode (loc has a
// file), errors return errScriptError to halt execution.
func replEval(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, input string, loc sourceLoc) error {
	// scriptErr prints an error and returns the appropriate
	// sentinel: errScriptError in file mode, nil in interactive
	// mode.
	scriptErr := func(format string, args ...any) error {
		_ = cli.PrintErrf(format, args...)
		if loc.file != "" {
			return errScriptError
		}
		return nil
	}

	tokens, err := shell.Tokenise(input)
	if err != nil {
		return scriptErr("%s[repl] error: %v\n", loc, err)
	}
	if len(tokens) == 0 {
		return nil
	}

	stmt, err := shell.ParseStmt(tokens)
	if err != nil {
		return scriptErr("%s[repl] error: %v\n", loc, err)
	}
	if stmt == nil {
		return nil
	}

	switch s := stmt.(type) {
	case *shell.SetStmt:
		expanded, err := session.Expand([]shell.Token{s.Value})
		if err != nil {
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		session.Set(s.Name, argToValue(expanded[0]))
		return nil

	case *shell.LetStmt:
		expanded, err := session.Expand(s.Command)
		if err != nil {
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		// Try shell commands first — some (like exec) produce
		// assignable values. Use WithDiscardOutput so that
		// bound commands do not print their normal output.
		var val shell.Value
		handled, val, err := replShellCmd(ctx, cli.WithDiscardOutput(), mgr, session, expanded, loc)
		if err != nil {
			if errors.Is(err, errRequireFailed) {
				return err
			}
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		if handled {
			if val.IsNil() {
				return scriptErr("%s[repl] error: cannot bind result of %q to a variable\n", loc, argText(expanded[0]))
			}
			session.Set(s.Name, val)
			return nil
		}
		val, err = replDispatch(ctx, cli, mgr, expanded)
		if err != nil {
			if errors.Is(err, errRequireFailed) {
				return err
			}
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		if val.IsNil() {
			return scriptErr("%s[repl] error: command produced no result to assign\n", loc)
		}
		session.Set(s.Name, val)
		return nil

	case *shell.CommandStmt:
		expanded, err := session.Expand(s.Tokens)
		if err != nil {
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		handled, _, err := replShellCmd(ctx, cli, mgr, session, expanded, loc)
		if err != nil {
			if errors.Is(err, errRequireFailed) {
				return err
			}
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		if handled {
			return nil
		}
		_, err = replDispatch(ctx, cli, mgr, expanded)
		if err != nil {
			if errors.Is(err, errRequireFailed) {
				return err
			}
			return scriptErr("%s[repl] error: %v\n", loc, err)
		}
		return nil

	default:
		return scriptErr("%s[repl] error: unknown statement type %T\n", loc, stmt)
	}
}

// argText extracts the text from a single Arg. For text-bearing
// variants (WordArg, QuotedArg, ScalarValueArg) this returns the
// text directly. For StructuredValueArg this returns "$name" as a
// display form suitable for error messages.
func argText(a shell.Arg) string {
	switch v := a.(type) {
	case shell.WordArg:
		return v.Text
	case shell.QuotedArg:
		return v.Text
	case shell.ScalarValueArg:
		return v.Text
	case shell.StructuredValueArg:
		return "$" + v.Name
	case shell.AdapterArg:
		if v.Path != "" {
			return fmt.Sprintf("%s:$%s.%s", v.Adapter, v.Name, v.Path)
		}
		return fmt.Sprintf("%s:$%s", v.Adapter, v.Name)
	default:
		return ""
	}
}

// argTexts extracts plain strings from all Args. This is the
// conversion boundary for passing expanded arguments to Kong parsers
// and handlers that operate on resolved string values. Structured
// values should already have been extracted by typed helpers before
// this point; any remaining StructuredValueArg is rendered as
// "$name" for display.
func argTexts(args []shell.Arg) []string {
	ss := make([]string, len(args))
	for i, a := range args {
		ss[i] = argText(a)
	}
	return ss
}

// argToValue converts a single Arg to a shell.Value for variable
// assignment. For structured args the Value is passed through
// directly; for all text-bearing args the text becomes a string
// value.
func argToValue(a shell.Arg) shell.Value {
	switch v := a.(type) {
	case shell.WordArg:
		return shell.StringValue(v.Text)
	case shell.QuotedArg:
		return shell.StringValue(v.Text)
	case shell.ScalarValueArg:
		return shell.StringValue(v.Text)
	case shell.StructuredValueArg:
		return v.Value
	default:
		return shell.StringValue("")
	}
}

// replCompleter returns a CompleteFunc that has access to the manager
// and session for dynamic completions such as program IDs and
// variable names.
func replCompleter(ctx context.Context, mgr *manager.Manager, session *shell.Session) CompleteFunc {
	return func(line string, pos int) (replace int, candidates []string) {
		return replComplete(ctx, mgr, session, line, pos)
	}
}

func replComplete(ctx context.Context, mgr *manager.Manager, session *shell.Session, line string, pos int) (replace int, candidates []string) {
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
		candidates = replFileCompletions("")
		return
	}
	if len(tokens) >= 2 && !trailingSpace {
		prevIdx := len(tokens) - 2
		if isPathFlag(tokens[prevIdx]) {
			prefix := tokens[len(tokens)-1]
			candidates = replFileCompletions(prefix)
			replace = len(prefix)
			return
		}
	}

	switch {
	case len(tokens) == 0 || (len(tokens) == 1 && !trailingSpace):
		// Completing the first token.
		prefix := ""
		if len(tokens) == 1 {
			prefix = tokens[0]
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
				candidates = replFileCompletions(tokens[1])
				replace = len(tokens[1])
			} else {
				candidates = replFileCompletions("")
			}
			return
		}
		// "dump" takes a variable path (bare, no $ prefix).
		if tokens[0] == "dump" {
			prefix := ""
			if len(tokens) == 2 {
				prefix = tokens[1]
			}
			candidates, replace = replCompleteVarPath(session, prefix, false)
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

// showProgramViews lists the valid sub-view names for "show program <id>".
var showProgramViews = []string{"links", "maps", "paths"}

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

// replCompleteVarPath completes dotted variable paths. The token is
// the partial text (e.g. "$prog.rec" or "prog.record."). When sigil
// is true, variable names carry a '$' prefix (program ID contexts);
// when false they are bare (dump context). Returns candidates and the
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
			candidate += varPathSuffix(v)
			candidates = append(candidates, candidate)
		}
		return
	}

	varName := stripped[:sepIdx]
	v, ok := session.Get(varName)
	if !ok {
		return
	}

	pathPart := stripped[sepIdx:]

	// Determine the resolved prefix (complete segments) and the
	// tail (partial last segment after the final '.' or '[').
	var resolvedPath, tail string
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

	// Map field completion: match keys against tail prefix.
	for _, key := range keys {
		if !strings.HasPrefix(key, tail) {
			continue
		}
		// Walk to the field to determine its trailing character.
		fieldPath := resolvedPath
		if fieldPath != "" {
			fieldPath += "." + key
		} else {
			fieldPath = key
		}
		child, err := v.LookupValue(varName, fieldPath)
		if err != nil {
			continue
		}
		candidate := candidatePrefix + "." + key + varPathSuffix(child)
		candidates = append(candidates, candidate)
	}
	return
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
// candidate based on the value type: "." for maps (invites deeper
// traversal), "[" for arrays (invites indexing), " " for scalars
// and nil (terminal).
func varPathSuffix(v shell.Value) string {
	switch v.Raw().(type) {
	case map[string]any:
		return "."
	case []any:
		return "["
	default:
		return " "
	}
}

// replFileCompletions returns filesystem path completions for the
// given prefix. Directories get a trailing slash. When the prefix
// starts with "./" the dot-slash is preserved in completions because
// filepath.Glob normalises it away.
func replFileCompletions(prefix string) []string {
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

	pattern := globPrefix + "*"
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil
	}
	var completions []string
	for _, m := range matches {
		// filepath.Glob strips the "./" prefix; restore it
		// when the user typed one.
		if dotSlash && !strings.HasPrefix(m, "./") {
			m = "./" + m
		}
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.IsDir() {
			completions = append(completions, m+"/")
		} else {
			completions = append(completions, m)
		}
	}
	return completions
}

type replContextKey int

const replSourcingKey replContextKey = iota

// replSource reads commands from a file and executes each line in the
// current session. The sourced file shares all variable bindings with
// the caller. Nested source commands are rejected to prevent
// unbounded recursion.
func replSource(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []string) error {
	if ctx.Value(replSourcingKey) != nil {
		return fmt.Errorf("source cannot be used inside a sourced file")
	}
	if len(args) != 1 {
		return fmt.Errorf("source requires exactly one file argument")
	}

	lr, err := openScriptReader(args[0])
	if err != nil {
		return err
	}
	defer lr.Close()

	ctx = context.WithValue(ctx, replSourcingKey, true)

	var lineNo int
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		lineNo++

		loc := sourceLoc{file: args[0], line: lineNo}
		if err := replEval(ctx, cli, mgr, session, input, loc); err != nil {
			return err
		}
	}
}

// replDispatch routes expanded domain command arguments to the
// appropriate bpfman command handler. Shell-language commands (assert,
// require, dump, help, source, unset, vars, version) are handled by
// replShellCmd before reaching this function.
//
// Parsing and execution are fully decoupled: parseCommand routes
// arguments to the per-command parser and returns a typed Command
// node, then execCommand dispatches execution via a type-switch.
func replDispatch(ctx context.Context, cli *CLI, mgr *manager.Manager, args []shell.Arg) (shell.Value, error) {
	cmd, err := parseCommand(args)
	if err != nil {
		return shell.Value{}, err
	}
	if cmd == nil {
		return shell.Value{}, nil
	}
	return execCommand(ctx, cli, mgr, cmd)
}

func replHelp(cli *CLI) error {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	b.WriteString("\n")
	b.WriteString("Program management:\n")
	b.WriteString("  program list [flags]                     List managed BPF programs\n")
	b.WriteString("  program get <id>                         Get program details (assignable)\n")
	b.WriteString("  program load file [flags]                Load from a local object file (assignable)\n")
	b.WriteString("  program load image [flags]               Load from an OCI image (assignable)\n")
	b.WriteString("  program unload <id>...                   Unload programs\n")
	b.WriteString("  program delete (<id>... | --all) [-r]    Delete with cascading cleanup\n")
	b.WriteString("  show program <id> [view] [-o]            Inspect (views: links, maps, paths)\n")
	b.WriteString("\n")
	b.WriteString("Link management:\n")
	b.WriteString("  link attach <type> [flags] <id>          Attach a program (assignable)\n")
	b.WriteString("  link detach <link-id>...                 Detach links\n")
	b.WriteString("  link get <link-id>                       Get link details (assignable)\n")
	b.WriteString("  link list [flags]                        List managed links\n")
	b.WriteString("  link delete <link-id>... [-r]            Delete with cascading cleanup\n")
	b.WriteString("\n")
	b.WriteString("Dispatcher management:\n")
	b.WriteString("  dispatcher list [--type <type>]           List dispatchers\n")
	b.WriteString("  dispatcher get <type> <nsid> <ifindex>    Get dispatcher details\n")
	b.WriteString("  dispatcher delete <type> <nsid> <ifindex> Delete a dispatcher\n")
	b.WriteString("\n")
	b.WriteString("Diagnostics:\n")
	b.WriteString("  gc [--dry-run] [--prune] [rule...]       Garbage collect stale resources\n")
	b.WriteString("  doctor [checkup]                         Run coherency checks\n")
	b.WriteString("  doctor explain [rule]                    Explain a coherency rule\n")
	b.WriteString("  version                                  Print version information\n")
	b.WriteString("\n")
	b.WriteString("Session:\n")
	b.WriteString("  exec <command> [args|file:$var...]        Run a host command (assignable)\n")
	b.WriteString("  exec status <command> [args...]           Run, capture all exit codes (assignable)\n")
	b.WriteString("  file temp <variable>[.path]               Write value to temp file (assignable)\n")
	b.WriteString("  json parse <string>                       Parse JSON into a value (assignable)\n")
	b.WriteString("  dump <variable>[.path]                    Display variable contents\n")
	b.WriteString("  source <file>                            Execute commands from a file\n")
	b.WriteString("  unset <var>...                           Remove variable bindings\n")
	b.WriteString("  vars                                     List session variables\n")
	b.WriteString("  help                                     Show this help\n")
	b.WriteString("\n")
	b.WriteString("Variables:\n")
	b.WriteString("  let prog = load file ...      Assign command result to a variable\n")
	b.WriteString("  set <name> = <value>          Bind scalar value to variable\n")
	b.WriteString("  show program $prog            Variable reference (auto-extracts program ID)\n")
	b.WriteString("  link attach xdp -i eth0 $prog Use $variable as program ID argument\n")
	b.WriteString("\n")
	b.WriteString("Assertions:\n")
	b.WriteString("  assert <verb> [args...]       Check condition, continue on failure\n")
	b.WriteString("  require <verb> [args...]      Check condition, stop on failure\n")
	b.WriteString("  assert not <verb> [args...]   Negate condition\n")
	b.WriteString("\n")
	b.WriteString("  Verbs: equal, ne, nil, not-empty, ok, fail, path exists,\n")
	b.WriteString("         contains, true, false, lt, le, gt, ge\n")
	return cli.PrintOut(b.String())
}

// replHistoryPath returns the path to the REPL history file,
// following the XDG Base Directory specification. The file is stored
// at $XDG_STATE_HOME/bpfman/repl-history, defaulting to
// $HOME/.local/state/bpfman/repl-history when XDG_STATE_HOME is
// unset. The parent directory is created if it does not exist.
func replHistoryPath() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine home directory: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(stateHome, "bpfman")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	return filepath.Join(dir, "repl-history"), nil
}

// replJSON implements the json shell command. Currently the only
// subcommand is "parse", which converts a JSON string into a
// structured shell.Value. In plain form the parsed value is printed
// as indented JSON. In bound form the value is returned for
// assignment.
func replJSON(cli *CLI, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 || argText(args[0]) != "parse" {
		return shell.Value{}, fmt.Errorf("usage: json parse <string>")
	}
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("json parse requires exactly one argument")
	}
	text := argText(args[1])
	val, err := shell.ValueFromJSON([]byte(text))
	if err != nil {
		return shell.Value{}, fmt.Errorf("json parse: %w", err)
	}
	b, err := json.MarshalIndent(val.Raw(), "", "  ")
	if err != nil {
		return shell.Value{}, fmt.Errorf("json parse: %w", err)
	}
	if err := cli.PrintOut(string(b) + "\n"); err != nil {
		return shell.Value{}, err
	}
	return val, nil
}

// replFile implements the file shell command. The only subcommand is
// "temp", which writes a REPL value to a private temporary file and
// returns the path as a scalar string. The argument is a bare
// variable name with optional field path (no $ prefix), the same
// form accepted by dump.
func replFile(cli *CLI, session *shell.Session, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 || argText(args[0]) != "temp" {
		return shell.Value{}, fmt.Errorf("usage: file temp <variable>[.path]")
	}
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("file temp requires exactly one argument")
	}
	v, err := lookupBareVar(session, argText(args[1]))
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	path, err := writeValueToTemp(v)
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	if err := cli.PrintOut(path + "\n"); err != nil {
		return shell.Value{}, err
	}
	return shell.StringValue(path), nil
}

// writeValueToTemp renders a shell.Value to a private temporary file
// and returns the absolute path. The file is created with mode 0600
// in the OS default temp directory with a recognisable prefix.
func writeValueToTemp(v shell.Value) (string, error) {
	data, err := shell.RenderValue(v)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "bpfman-repl-")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	return f.Name(), nil
}

// execResult holds the captured output of a subprocess run by the
// exec shell command. The JSON tags produce the field names visible
// in the REPL's structured-value model.
type execResult struct {
	Argv     []string `json:"argv"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit_code"`
}

// replExec runs an external command and returns a structured result.
//
// In strict mode (the default), exit 0 returns a structured result
// and non-zero exit returns an error. This keeps the common case
// clean for require ok exec and assert ok exec.
//
// In status mode (exec status ...), non-zero exit is not an error.
// The structured result is returned for all exit codes, with
// exit_code reflecting the actual status. Only genuine launch
// failures (command not found, permission denied) produce errors.
// This mode is for commands like diff, grep -q, and cmp where
// non-zero exit is a domain result rather than an execution failure.
//
// Inline adapter arguments (e.g. file:$var.path) are resolved to
// temporary files before the command runs. All adapter-created temp
// files are removed unconditionally after the command completes.
func replExec(ctx context.Context, cli *CLI, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("exec requires at least one argument")
	}

	// Detect status mode.
	statusMode := false
	if argText(args[0]) == "status" {
		statusMode = true
		args = args[1:]
		if len(args) == 0 {
			return shell.Value{}, fmt.Errorf("exec status requires at least one argument")
		}
	}

	// Resolve adapter args to temp files.
	var tempFiles []string
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		aa, ok := a.(shell.AdapterArg)
		if !ok {
			resolved[i] = a
			continue
		}
		if aa.Adapter != "file" {
			return shell.Value{}, fmt.Errorf("unknown adapter %q", aa.Adapter)
		}
		path, err := writeValueToTemp(aa.Value)
		if err != nil {
			return shell.Value{}, fmt.Errorf("adapter file: %w", err)
		}
		tempFiles = append(tempFiles, path)
		resolved[i] = shell.ScalarValueArg{Text: path}
	}

	argv := argTexts(resolved)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// Not an exit error — command not found or similar.
			return shell.Value{}, fmt.Errorf("exec %s: %w", argv[0], err)
		}
		if !statusMode {
			msg := fmt.Sprintf("exec %s: exit status %d", strings.Join(argv, " "), exitErr.ExitCode())
			if stderr.Len() > 0 {
				msg += ": " + strings.TrimRight(stderr.String(), "\n")
			}
			return shell.Value{}, errors.New(msg)
		}
		exitCode = exitErr.ExitCode()
	}

	result := execResult{
		Argv:     argv,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
	val, err := shell.ValueFromStruct(result)
	if err != nil {
		return shell.Value{}, fmt.Errorf("exec: build result: %w", err)
	}

	if stdout.Len() > 0 {
		if err := cli.PrintOut(stdout.String()); err != nil {
			return shell.Value{}, err
		}
	}

	return val, nil
}

// replVars lists all session variables and their types.
func replVars(cli *CLI, session *shell.Session) error {
	names := session.Names()
	if len(names) == 0 {
		return cli.PrintOut("No variables defined.\n")
	}
	var b strings.Builder
	for _, name := range names {
		v, _ := session.Get(name)
		kind := "scalar"
		if v.IsStructured() {
			kind = "structured"
		}
		fmt.Fprintf(&b, "  %s (%s)\n", name, kind)
	}
	return cli.PrintOut(b.String())
}

// replUnset removes one or more variable bindings from the session.
func replUnset(cli *CLI, session *shell.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("unset requires at least one variable name")
	}
	for _, name := range args {
		if _, ok := session.Get(name); !ok {
			return fmt.Errorf("undefined variable %q", name)
		}
		session.Delete(name)
	}
	return nil
}

// replDump displays the contents of a session variable. The argument
// is a bare variable name with an optional dotted/indexed path (no $
// prefix). Scalars are printed as plain text, structured values as
// indented JSON, and nil as "null".
func replDump(cli *CLI, session *shell.Session, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("dump requires exactly one argument: dump <variable>[.path]")
	}

	v, err := lookupBareVar(session, args[0])
	if err != nil {
		return err
	}

	if v.IsNil() {
		return cli.PrintOut("null\n")
	}

	if v.IsScalar() {
		s, err := v.Scalar()
		if err != nil {
			return err
		}
		return cli.PrintOut(s + "\n")
	}

	b, err := json.MarshalIndent(v.Raw(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	return cli.PrintOut(string(b) + "\n")
}

// lookupBareVar resolves a bare variable name (no $ prefix) with an
// optional dotted path into a shell.Value. This is the shared logic
// used by dump and assert nil.
func lookupBareVar(session *shell.Session, arg string) (shell.Value, error) {
	varName := arg
	path := ""
	if i := strings.IndexAny(arg, ".["); i >= 0 {
		varName = arg[:i]
		path = arg[i:]
		path = strings.TrimPrefix(path, ".")
	}

	v, ok := session.Get(varName)
	if !ok {
		return shell.Value{}, fmt.Errorf("undefined variable %q", varName)
	}

	if path != "" {
		return v.LookupValue(varName, path)
	}
	return v, nil
}

// replAssertRequire handles both "assert" and "require" commands.
// When isRequire is true, failure halts execution immediately via
// errRequireFailed. When false, failure is recorded in the session
// counter and execution continues.
func replAssertRequire(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, isRequire bool, loc sourceLoc) error {
	if len(args) == 0 {
		return fmt.Errorf("expected a verb (equal, ne, nil, not-empty, ok, fail, path, contains, true, false, lt, le, gt, ge)")
	}

	label := "assert"
	if isRequire {
		label = "require"
	}

	// Check for "not" negation.
	negate := false
	if argText(args[0]) == "not" {
		negate = true
		args = args[1:]
		if len(args) == 0 {
			return fmt.Errorf("expected a verb after \"not\"")
		}
	}

	verb := argText(args[0])
	verbArgs := args[1:]

	result, err := evalAssertVerb(ctx, cli, mgr, session, verb, verbArgs)
	if err != nil {
		return err
	}

	if negate {
		result.pass = !result.pass
		result.message = negateMessage(result.message)
	}

	if result.pass {
		return nil
	}

	// Failure path.
	_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, result.message)

	if isRequire {
		return errRequireFailed
	}

	session.RecordAssertFailure()
	return nil
}

// evalAssertVerb dispatches to the appropriate verb evaluator.
func evalAssertVerb(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, verb string, args []shell.Arg) (assertResult, error) {
	ss := argTexts(args)
	switch verb {
	case "equal":
		return assertEqual(ss)
	case "ne":
		return assertNe(ss)
	case "nil":
		return assertNil(session, ss)
	case "not-empty":
		return assertNotEmpty(ss)
	case "ok":
		return assertOk(ctx, cli, mgr, session, args)
	case "fail":
		return assertFail(ctx, cli, mgr, session, args)
	case "path":
		return assertPath(ss)
	case "contains":
		return assertContains(ss)
	case "true":
		return assertBool(ss, true)
	case "false":
		return assertBool(ss, false)
	case "lt":
		return assertNumericCmp(ss, "lt")
	case "le":
		return assertNumericCmp(ss, "le")
	case "gt":
		return assertNumericCmp(ss, "gt")
	case "ge":
		return assertNumericCmp(ss, "ge")
	default:
		return assertResult{}, fmt.Errorf("unknown assertion verb %q", verb)
	}
}

func assertEqual(args []string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, fmt.Errorf("equal requires exactly 2 arguments")
	}
	pass := args[0] == args[1]
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %q to equal %q", args[0], args[1]),
	}, nil
}

func assertNe(args []string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, fmt.Errorf("ne requires exactly 2 arguments")
	}
	pass := args[0] != args[1]
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %q to not equal %q", args[0], args[1]),
	}, nil
}

func assertNil(session *shell.Session, args []string) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, fmt.Errorf("nil requires exactly 1 argument (bare variable name, no $)")
	}
	v, err := lookupBareVar(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    v.IsNil(),
		message: fmt.Sprintf("expected %s to be nil", args[0]),
	}, nil
}

func assertNotEmpty(args []string) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, fmt.Errorf("not-empty requires exactly 1 argument")
	}
	pass := args[0] != ""
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected non-empty string, got %q", args[0]),
	}, nil
}

// runCommand executes a command through both the shell command layer
// and the domain dispatch layer. It is used by assertion verbs (ok,
// fail) to test whether a sub-command succeeds or fails.
func runCommand(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) error {
	handled, _, err := replShellCmd(ctx, cli, mgr, session, args, sourceLoc{})
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	_, err = replDispatch(ctx, cli, mgr, args)
	return err
}

func assertOk(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, fmt.Errorf("ok requires a command")
	}
	err := runCommand(ctx, cli.WithDiscardOutput(), mgr, session, args)
	if err != nil {
		return assertResult{
			pass:    false,
			message: fmt.Sprintf("expected command to succeed, but got: %v", err),
		}, nil
	}
	return assertResult{
		pass:    true,
		message: "expected command to succeed",
	}, nil
}

func assertFail(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, fmt.Errorf("fail requires a command")
	}
	err := runCommand(ctx, cli.WithDiscardOutput(), mgr, session, args)
	if err != nil {
		return assertResult{
			pass:    true,
			message: "expected command to fail",
		}, nil
	}
	return assertResult{
		pass:    false,
		message: "expected command to fail, but it succeeded",
	}, nil
}

func assertPath(args []string) (assertResult, error) {
	if len(args) != 2 || args[0] != "exists" {
		return assertResult{}, fmt.Errorf("path requires: path exists <filepath>")
	}
	_, err := os.Stat(args[1])
	pass := err == nil
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected path %q to exist", args[1]),
	}, nil
}

func assertContains(args []string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, fmt.Errorf("contains requires exactly 2 arguments: <haystack> <needle>")
	}
	pass := strings.Contains(args[0], args[1])
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %q to contain %q", args[0], args[1]),
	}, nil
}

func assertBool(args []string, want bool) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, fmt.Errorf("true/false requires exactly 1 argument")
	}
	wantStr := strconv.FormatBool(want)
	pass := args[0] == wantStr
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %q to be %q", args[0], wantStr),
	}, nil
}

func assertNumericCmp(args []string, op string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, fmt.Errorf("%s requires exactly 2 numeric arguments", op)
	}
	a, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		return assertResult{}, fmt.Errorf("%s: left argument %q is not a number", op, args[0])
	}
	b, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return assertResult{}, fmt.Errorf("%s: right argument %q is not a number", op, args[1])
	}

	var pass bool
	var symbol string
	switch op {
	case "lt":
		pass = a < b
		symbol = "<"
	case "le":
		pass = a <= b
		symbol = "<="
	case "gt":
		pass = a > b
		symbol = ">"
	case "ge":
		pass = a >= b
		symbol = ">="
	}

	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %s %s %s", args[0], symbol, args[1]),
	}, nil
}

// negateMessage transforms an assertion message for negated assertions.
// It inserts "not" into the message: "expected X to equal Y" becomes
// "expected X not to equal Y", "expected X to be Y" becomes
// "expected X not to be Y".
func negateMessage(msg string) string {
	// Try "to equal", "to not equal", "to be", "to contain", "to exist", "to succeed", "to fail".
	if i := strings.Index(msg, " to "); i >= 0 {
		return msg[:i] + " not to " + msg[i+4:]
	}
	// Try "expected command to" patterns.
	if strings.HasPrefix(msg, "expected") {
		return "expected not: " + msg[9:]
	}
	return "not: " + msg
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

// replVersion prints version information.
func replVersion(cli *CLI) error {
	return cli.PrintOut(version.Get().Long())
}

func replDoctorCheckup(ctx context.Context, cli *CLI, mgr *manager.Manager) error {
	report, err := mgr.Doctor(ctx)
	if err != nil {
		return fmt.Errorf("doctor failed: %w", err)
	}

	if len(report.Findings) == 0 {
		return cli.PrintOut("All checks passed. Database, kernel, and filesystem are coherent.\n")
	}

	ruleCounts := make(map[string]int)
	for _, f := range report.Findings {
		ruleCounts[f.RuleName]++
	}

	var out strings.Builder
	var errorCount, warningCount int
	lastCategory := ""
	lastRule := ""

	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)

	for _, f := range report.Findings {
		category := categoryHeading(f.Category)
		if category != lastCategory {
			w.Flush()
			if lastCategory != "" {
				out.WriteString("\n")
			}
			out.WriteString(category)
			out.WriteString("\n")
			lastCategory = category
			lastRule = ""
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		if f.RuleName != lastRule {
			w.Flush()
			fmt.Fprintf(&out, "  [%s] (%d)\n", f.RuleName, ruleCounts[f.RuleName])
			lastRule = f.RuleName
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		fmt.Fprintf(w, "    %s\t%s\n", f.Severity, f.Description)
		switch f.Severity {
		case coherency.SeverityError:
			errorCount++
		case coherency.SeverityWarning:
			warningCount++
		}
	}
	w.Flush()

	fmt.Fprintf(&out, "\nSummary: %d error(s), %d warning(s)\n", errorCount, warningCount)
	return cli.PrintOut(out.String())
}

func replDoctorExplain(cli *CLI, args []string) error {
	if len(args) == 0 {
		// List all rules.
		var out strings.Builder
		out.WriteString("Available coherency rules:\n\n")
		names := coherency.RuleNames()
		sort.Strings(names)
		for _, name := range names {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString("\n")
		}
		out.WriteString("\nUse 'doctor explain <rule>' for details.\n")
		return cli.PrintOut(out.String())
	}

	ruleName := args[0]
	rule := coherency.FindRule(ruleName)
	if rule == nil {
		return fmt.Errorf("unknown rule: %s\n\nUse 'doctor explain' to list all rules", ruleName)
	}

	var out strings.Builder
	out.WriteString(rule.Name)
	out.WriteString("\n")
	out.WriteString(strings.Repeat("=", len(rule.Name)))
	out.WriteString("\n\n")
	if rule.Description != "" {
		out.WriteString(rule.Description)
	} else {
		out.WriteString("(No description available)")
	}
	out.WriteString("\n")
	return cli.PrintOut(out.String())
}
