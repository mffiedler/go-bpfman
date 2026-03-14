package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/alecthomas/kong"
	"golang.org/x/term"

	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/replang"
)

// ReplCmd starts an interactive shell for inspecting BPF state.
// When --file is given, commands are read from the named file. When
// stdin is not a terminal, commands are read from stdin. Otherwise an
// interactive readline prompt is started.
type ReplCmd struct {
	File string `name:"file" short:"f" help:"Read commands from a file (use '-' for stdin)."`
}

// replCommandNames lists the top-level command tokens for completion.
var replCommandNames = []string{"help", "list", "load", "program", "programs", "show", "source", "vars"}

// replSubcommands maps a top-level token to its valid subcommands for completion.
var replSubcommands = map[string][]string{
	"list":     {"programs"},
	"load":     {"file"},
	"program":  {"delete", "list", "load"},
	"programs": {"list"},
	"show":     {"program"},
}

// Run starts the read-eval-print loop. A single manager is held open
// for the session lifetime to avoid repeated store open/close.
func (c *ReplCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	lr, err := c.newReader(ctx, mgr)
	if err != nil {
		return err
	}
	defer lr.Close()

	return replLoop(ctx, cli, mgr, lr)
}

// newReader selects the appropriate LineReader: file, pipe, or
// interactive readline.
func (c *ReplCmd) newReader(ctx context.Context, mgr *manager.Manager) (LineReader, error) {
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
	return NewLineReader("bpfman> ", historyPath, replCompleter(ctx, mgr))
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

// replLoop reads lines from lr and dispatches them until EOF or
// interrupt. Blank lines and comments are handled by
// replang.Tokenise. Variable assignment and expansion use the
// replang.Session.
func replLoop(ctx context.Context, cli *CLI, mgr *manager.Manager, lr LineReader) error {
	session := replang.NewSession()

	for {
		input, err := lr.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				return nil
			}
			return err
		}

		if err := replEval(ctx, cli, mgr, session, input); err != nil {
			return err
		}
	}
}

// replEval processes a single input line: tokenise, parse, expand
// variables, dispatch, and optionally bind the result. Non-fatal
// errors (unknown command, undefined variable, failed assignment) are
// printed to cli.Err and return nil, matching the interactive
// continue behaviour. Only unexpected errors are returned.
func replEval(ctx context.Context, cli *CLI, mgr *manager.Manager, session *replang.Session, input string) error {
	tokens, err := replang.Tokenise(input)
	if err != nil {
		_ = cli.PrintErrf("[repl] error: %v\n", err)
		return nil
	}
	if len(tokens) == 0 {
		return nil
	}

	line, err := replang.ParseLine(tokens)
	if err != nil {
		_ = cli.PrintErrf("[repl] error: %v\n", err)
		return nil
	}

	expanded, err := session.Expand(line.Command)
	if err != nil {
		_ = cli.PrintErrf("[repl] error: %v\n", err)
		return nil
	}

	args := tokenTexts(expanded)
	val, err := replDispatch(ctx, cli, mgr, session, args)
	if err != nil {
		_ = cli.PrintErrf("[repl] error: %v\n", err)
		return nil
	}

	if line.VarName != "" {
		if val.IsNil() {
			_ = cli.PrintErrf("[repl] error: command produced no result to assign\n")
			return nil
		}
		session.Set(line.VarName, val)
	}

	return nil
}

// tokenTexts extracts the text of each token into a plain string
// slice. This bridges replang.Token to the []string that Kong parsers
// and command handlers expect.
func tokenTexts(tokens []replang.Token) []string {
	ss := make([]string, len(tokens))
	for i, t := range tokens {
		ss[i] = t.Text
	}
	return ss
}

// replCompleter returns a CompleteFunc that has access to the manager
// for dynamic completions such as program IDs.
func replCompleter(ctx context.Context, mgr *manager.Manager) CompleteFunc {
	return func(line string, pos int) (replace int, candidates []string) {
		return replComplete(ctx, mgr, line, pos)
	}
}

func replComplete(ctx context.Context, mgr *manager.Manager, line string, pos int) (replace int, candidates []string) {
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
		candidates, replace = replCompleteArgs(ctx, mgr, tokens, trailingSpace)
	}

	return
}

// replCompleteArgs handles completion for the third token onwards,
// dispatching based on the command prefix.
func replCompleteArgs(ctx context.Context, mgr *manager.Manager, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	if len(tokens) < 2 {
		return
	}
	if tokens[0] == "program" && tokens[1] == "delete" {
		return replCompleteProgramIDs(ctx, mgr, tokens[2:], trailingSpace)
	}
	if tokens[0] == "show" && tokens[1] == "program" {
		return replCompleteShowProgram(ctx, mgr, tokens[2:], trailingSpace)
	}
	return
}

// showProgramViews lists the valid sub-view names for "show program <id>".
var showProgramViews = []string{"links", "maps", "paths"}

// replCompleteShowProgram handles completion for "show program ..."
// arguments. The first argument is a program ID; the second is a
// sub-view name.
func replCompleteShowProgram(ctx context.Context, mgr *manager.Manager, args []string, trailingSpace bool) (candidates []string, replace int) {
	switch {
	case len(args) == 0 && trailingSpace:
		// "show program " -- complete program IDs
		return replCompleteProgramIDs(ctx, mgr, args, trailingSpace)
	case len(args) == 1 && !trailingSpace:
		// "show program 12" -- partial program ID
		return replCompleteProgramIDs(ctx, mgr, args, trailingSpace)
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

// replCompleteProgramIDs offers program ID completions, excluding IDs
// that have already been specified on the command line.
func replCompleteProgramIDs(ctx context.Context, mgr *manager.Manager, args []string, trailingSpace bool) (candidates []string, replace int) {
	result, err := mgr.ListPrograms(ctx)
	if err != nil {
		return
	}

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

	for _, prog := range result.Programs {
		id := fmt.Sprintf("%d", prog.Record.ProgramID)
		if _, already := specified[id]; already {
			continue
		}
		if strings.HasPrefix(id, prefix) {
			candidates = append(candidates, id+" ")
		}
	}
	replace = len(prefix)
	return
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
func replSource(ctx context.Context, cli *CLI, mgr *manager.Manager, session *replang.Session, args []string) error {
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

	for {
		input, err := lr.Readline()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if err := replEval(ctx, cli, mgr, session, input); err != nil {
			return err
		}
	}
}

func replDispatch(ctx context.Context, cli *CLI, mgr *manager.Manager, session *replang.Session, args []string) (replang.Value, error) {
	if len(args) == 0 {
		return replang.Value{}, nil
	}

	switch {
	case args[0] == "help":
		return replang.Value{}, replHelp(cli)
	case args[0] == "source":
		return replang.Value{}, replSource(ctx, cli, mgr, session, args[1:])
	case args[0] == "vars":
		return replang.Value{}, replVars(cli, session)
	case len(args) >= 2 && args[0] == "list" && args[1] == "programs":
		return replang.Value{}, replListPrograms(ctx, cli, mgr)
	case len(args) >= 2 && (args[0] == "program" || args[0] == "programs") && args[1] == "list":
		return replang.Value{}, replListPrograms(ctx, cli, mgr)
	case len(args) >= 2 && args[0] == "load" && args[1] == "file":
		return replLoadFile(ctx, cli, mgr, args[2:])
	case len(args) >= 3 && args[0] == "program" && args[1] == "load" && args[2] == "file":
		return replLoadFile(ctx, cli, mgr, args[3:])
	case len(args) >= 3 && args[0] == "program" && args[1] == "delete":
		return replang.Value{}, replDeleteProgram(ctx, cli, mgr, args[2:])
	case len(args) >= 3 && args[0] == "show" && args[1] == "program":
		return replang.Value{}, replShowProgram(ctx, cli, mgr, args[2:])
	default:
		return replang.Value{}, fmt.Errorf("unknown command %q. Type \"help\" for available commands.", strings.Join(args, " "))
	}
}

func replHelp(cli *CLI) error {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	b.WriteString("  help                          Show this help\n")
	b.WriteString("  list programs                 List managed BPF programs\n")
	b.WriteString("  load file [flags]             Load a BPF program from a local object file\n")
	b.WriteString("  program delete <id>... [-r]   Delete programs with cascading cleanup\n")
	b.WriteString("  program list                  Alias for list programs\n")
	b.WriteString("  show program <id> [view] [-o]  Inspect program (views: links, maps, paths)\n")
	b.WriteString("  source <file>                 Execute commands from a file\n")
	b.WriteString("  vars                          List session variables\n")
	b.WriteString("\n")
	b.WriteString("Variables:\n")
	b.WriteString("  prog = load file ...          Assign command result to a variable\n")
	b.WriteString("  show program $prog.record.program_id  Use variable fields in commands\n")
	b.WriteString("\n")
	b.WriteString("Load file flags:\n")
	b.WriteString("  --path, -p PATH             Path to the BPF object file (.o) (required)\n")
	b.WriteString("  --programs TYPE:NAME[:FUNC]  Program to load (can be repeated)\n")
	b.WriteString("  -m KEY=VALUE                Metadata to attach (can be repeated)\n")
	b.WriteString("  -g NAME=HEX                 Global data (can be repeated)\n")
	b.WriteString("  -a, --application NAME       Application name\n")
	b.WriteString("  --map-owner-id ID            Program ID to share maps with\n")
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

// replVars lists all session variables and their types.
func replVars(cli *CLI, session *replang.Session) error {
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

func replListPrograms(ctx context.Context, cli *CLI, mgr *manager.Manager) error {
	result, err := mgr.ListPrograms(ctx)
	if err != nil {
		return err
	}
	flags := &OutputFlags{Output: OutputValue{Value: "table"}}
	output, err := FormatProgramsComposite(result, flags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// replParseLoadFile parses REPL tokens into a LoadFileCmd using a
// scoped Kong parser. This reuses the existing type mappers for
// ProgramSpec, GlobalData, KeyValue, and ObjectPath.
func replParseLoadFile(args []string) (*LoadFileCmd, error) {
	var cmd LoadFileCmd
	parser, err := kong.New(&cmd,
		kong.Name("load file"),
		kong.Description("Load a BPF program from a local object file."),
		kong.Exit(func(int) {}),
		kong.Writers(io.Discard, io.Discard),
		kong.TypeMapper(reflect.TypeOf(KeyValue{}), keyValueMapper()),
		kong.TypeMapper(reflect.TypeOf(GlobalData{}), globalDataMapper()),
		kong.TypeMapper(reflect.TypeOf(ProgramSpec{}), programSpecMapper()),
		kong.TypeMapper(reflect.TypeOf(OutputValue{}), outputValueMapper()),
	)
	if err != nil {
		return nil, fmt.Errorf("create parser: %w", err)
	}
	_, err = parser.Parse(args)
	if err != nil {
		return nil, err
	}
	return &cmd, nil
}

// replLoadFileExec parses and executes a load file command, returning
// the result for both display and optional variable assignment.
func replLoadFileExec(ctx context.Context, cli *CLI, mgr *manager.Manager, args []string) (loadFileResult, *LoadFileCmd, error) {
	cmd, err := replParseLoadFile(args)
	if err != nil {
		return loadFileResult{}, nil, err
	}
	result, err := executeLoadFileResult(ctx, cli, mgr, cmd)
	if err != nil {
		return loadFileResult{}, nil, err
	}
	return result, cmd, nil
}

// replLoadFile handles the "load file" REPL command by parsing the
// remaining tokens, executing the load, printing output, and
// returning a structured Value for optional assignment.
func replLoadFile(ctx context.Context, cli *CLI, mgr *manager.Manager, args []string) (replang.Value, error) {
	result, cmd, err := replLoadFileExec(ctx, cli, mgr, args)
	if err != nil {
		return replang.Value{}, err
	}

	output, err := FormatLoadedPrograms(result.Programs, &cmd.OutputFlags)
	if err != nil {
		return replang.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return replang.Value{}, err
	}

	if len(result.Programs) == 0 {
		return replang.Value{}, nil
	}

	val, err := replang.ValueFromStruct(result.Programs[0])
	if err != nil {
		return replang.Value{}, nil
	}
	return val, nil
}

// replParseDeleteProgram parses REPL tokens into a ProgramDeleteCmd.
func replParseDeleteProgram(args []string) (*ProgramDeleteCmd, error) {
	var cmd ProgramDeleteCmd
	parser, err := kong.New(&cmd,
		kong.Name("program delete"),
		kong.Description("Delete programs with cascading cleanup."),
		kong.Exit(func(int) {}),
		kong.Writers(io.Discard, io.Discard),
		kong.TypeMapper(reflect.TypeOf(ProgramID{}), programIDMapper()),
		kong.TypeMapper(reflect.TypeOf(OutputValue{}), outputValueMapper()),
	)
	if err != nil {
		return nil, fmt.Errorf("create parser: %w", err)
	}
	_, err = parser.Parse(args)
	if err != nil {
		return nil, err
	}
	return &cmd, nil
}

// ShowProgramCmd is the Kong-parsed structure for "show program".
type ShowProgramCmd struct {
	ID     ProgramID   `arg:"" help:"Program ID to inspect."`
	View   string      `arg:"" optional:"" default:"summary" help:"Sub-view: summary, links, maps, paths."`
	Output OutputFlags `embed:""`
}

// replParseShowProgram parses REPL tokens into a ShowProgramCmd.
func replParseShowProgram(args []string) (*ShowProgramCmd, error) {
	var cmd ShowProgramCmd
	parser, err := kong.New(&cmd,
		kong.Name("show program"),
		kong.Description("Inspect a managed BPF program."),
		kong.Exit(func(int) {}),
		kong.Writers(io.Discard, io.Discard),
		kong.TypeMapper(reflect.TypeOf(ProgramID{}), programIDMapper()),
		kong.TypeMapper(reflect.TypeOf(OutputValue{}), outputValueMapper()),
	)
	if err != nil {
		return nil, fmt.Errorf("create parser: %w", err)
	}
	_, err = parser.Parse(args)
	if err != nil {
		return nil, err
	}
	return &cmd, nil
}

// replShowProgram handles the "show program" REPL command.
func replShowProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, args []string) error {
	cmd, err := replParseShowProgram(args)
	if err != nil {
		return err
	}

	detail, err := buildProgramDetail(ctx, mgr, cmd.ID.Value)
	if err != nil {
		return err
	}

	format, err := cmd.Output.Format()
	if err != nil {
		return err
	}

	// JSON output always emits the full ProgramDetail regardless
	// of sub-view; consumers can select fields with jq.
	if format == OutputFormatJSON {
		output, err := formatShowJSON(detail)
		if err != nil {
			return err
		}
		return cli.PrintOut(output)
	}

	var output string
	switch cmd.View {
	case "summary":
		output = formatShowSummary(detail)
	case "links":
		output = formatShowLinks(detail)
	case "maps":
		output = formatShowMaps(detail)
	case "paths":
		output = formatShowPaths(detail)
	default:
		return fmt.Errorf("unknown view %q (valid: summary, links, maps, paths)", cmd.View)
	}

	return cli.PrintOut(output)
}

// replDeleteProgram handles the "program delete" REPL command.
func replDeleteProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, args []string) error {
	cmd, err := replParseDeleteProgram(args)
	if err != nil {
		return err
	}

	ids := make([]kernel.ProgramID, len(cmd.ProgramIDs))
	for i, pid := range cmd.ProgramIDs {
		ids[i] = pid.Value
	}
	return executeDeletePrograms(ctx, cli, mgr, ids, cmd.Recursive)
}
