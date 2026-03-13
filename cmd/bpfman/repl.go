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
)

// ReplCmd starts an interactive shell for inspecting BPF state.
// When --file is given, commands are read from the named file. When
// stdin is not a terminal, commands are read from stdin. Otherwise an
// interactive readline prompt is started.
type ReplCmd struct {
	File string `name:"file" short:"f" help:"Read commands from a file (use '-' for stdin)."`
}

// replCommandNames lists the top-level command tokens for completion.
var replCommandNames = []string{"help", "list", "load", "program", "programs"}

// replSubcommands maps a top-level token to its valid subcommands for completion.
var replSubcommands = map[string][]string{
	"list":     {"programs"},
	"load":     {"file"},
	"program":  {"delete", "list", "load"},
	"programs": {"list"},
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
// interrupt. Blank lines are skipped. A '#' and everything after it
// is treated as a comment and stripped, following shell conventions.
func replLoop(ctx context.Context, cli *CLI, mgr *manager.Manager, lr LineReader) error {
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				return nil
			}
			return err
		}

		if i := strings.IndexByte(input, '#'); i >= 0 {
			input = input[:i]
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		if err := replDispatch(ctx, cli, mgr, input); err != nil {
			_ = cli.PrintErrf("[repl] error: %v\n", err)
		}
	}
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

func replDispatch(ctx context.Context, cli *CLI, mgr *manager.Manager, input string) error {
	tokens := strings.Fields(input)
	if len(tokens) == 0 {
		return nil
	}

	switch {
	case tokens[0] == "help":
		return replHelp(cli)
	case len(tokens) >= 2 && tokens[0] == "list" && tokens[1] == "programs":
		return replListPrograms(ctx, cli, mgr)
	case len(tokens) >= 2 && (tokens[0] == "program" || tokens[0] == "programs") && tokens[1] == "list":
		return replListPrograms(ctx, cli, mgr)
	case len(tokens) >= 2 && tokens[0] == "load" && tokens[1] == "file":
		return replLoadFile(ctx, cli, mgr, tokens[2:])
	case len(tokens) >= 3 && tokens[0] == "program" && tokens[1] == "load" && tokens[2] == "file":
		return replLoadFile(ctx, cli, mgr, tokens[3:])
	case len(tokens) >= 3 && tokens[0] == "program" && tokens[1] == "delete":
		return replDeleteProgram(ctx, cli, mgr, tokens[2:])
	default:
		return fmt.Errorf("unknown command %q. Type \"help\" for available commands.", input)
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

// replLoadFile handles the "load file" REPL command by parsing the
// remaining tokens and delegating to the shared load implementation.
func replLoadFile(ctx context.Context, cli *CLI, mgr *manager.Manager, args []string) error {
	cmd, err := replParseLoadFile(args)
	if err != nil {
		return err
	}
	return executeLoadFile(ctx, cli, mgr, cmd)
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
