// REPL entry shim and bpfman-specific pieces that the repl
// framework hooks into: the domain-command bridge (replDispatch +
// domainNouns), the bind-side fast paths (wait, net exec), and
// the help / version handlers.

package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/version"
)

// Run is the CLI's top-level entry. With --check / --ast it
// short-circuits to those parse-only pipelines; otherwise it
// opens the manager, builds a repl.Config, and delegates to
// repl.Run.
func (c *CLI) Run(ctx context.Context) error {
	if c.Check {
		return c.runCheck()
	}
	if c.AST {
		return c.runAST()
	}
	mgr, cleanup, err := c.NewManagerWithPuller(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	session := shell.NewSession()
	if c.Trace {
		session.SetTrace(true)
	}

	lr, err := c.newReader(ctx, mgr, session)
	if err != nil {
		return err
	}
	defer lr.Close()

	// Three input shapes:
	//   --script <FILE>      file != "" (the named script).
	//   stdin pipe / -       file = "<stdin>" (still a script
	//                        contract, just from stdin).
	//   bare TTY invocation  file = "" and interactive = true.
	file := c.Script
	interactive := false
	switch {
	case file == "-":
		file = "<stdin>"
	case file == "" && !term.IsTerminal(int(os.Stdin.Fd())):
		file = "<stdin>"
	case file == "":
		interactive = true
	}

	return repl.Run(ctx, repl.Config{
		CLI:            &c.CLI,
		Mgr:            mgr,
		LineReader:     lr,
		Session:        session,
		File:           file,
		Interactive:    interactive,
		NoCheck:        c.NoCheck,
		Fallback:       bpfmanFallback,
		BindFallback:   bpfmanBindFallback,
		MakeAssertStmt: makeExecAssertStmt,
		PromptPrimary:  bpfmanShellPromptPrimary,
		PromptContinue: bpfmanShellPromptContinue,
	})
}

// bpfmanShellPromptPrimary and bpfmanShellPromptContinue are the
// product-specific prompt strings the interactive loop displays.
// Kept here in the binary rather than in repl/ so the framework
// has no hardcoded product name.
const (
	bpfmanShellPromptPrimary  = "bpfman> "
	bpfmanShellPromptContinue = "... "
)

// newReader selects the appropriate LineReader: positional
// script file, piped stdin, or interactive readline.
func (c *CLI) newReader(ctx context.Context, mgr *manager.Manager, session *shell.Session) (repl.LineReader, error) {
	if c.Script != "" {
		return repl.OpenScriptReader(c.Script)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return repl.NewScannerReader(os.Stdin, nil), nil
	}
	historyPath, err := replHistoryPath()
	if err != nil {
		return nil, fmt.Errorf("history path: %w", err)
	}
	return repl.NewLineReader(bpfmanShellPromptPrimary, historyPath, replCompleter(ctx, mgr, session))
}

// domainNouns is the set of top-level words that parseCommand
// recognises after a leading "bpfman". The bpfman bridge uses it
// to distinguish "forgot the bpfman prefix" (suggest prefixing)
// from "this word is not a command at all" (run as external).
var domainNouns = map[string]bool{
	"program":    true,
	"show":       true,
	"link":       true,
	"dispatcher": true,
	"audit":      true,
}

// bpfmanFallback is the statement-position fallback the repl
// loop calls when no registered builtin matches the first
// token. It owns the "bpfman ..." dispatch and the
// "forgot the bpfman prefix" diagnostic; unknown first words
// fall through to external execution (handled == false).
func bpfmanFallback(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []shell.Arg, loc repl.SourceLoc, span shell.Span) (bool, shell.Value, error) {
	if len(args) == 0 {
		return false, shell.Value{}, nil
	}
	first := repl.ArgText(args[0])
	if first == "bpfman" {
		val, err := replDispatch(ctx, cli, mgr, args)
		if err != nil {
			// bpfman dispatch failures are runtime outcomes
			// on a well-formed construct: cite, do not frame.
			return true, val, &repl.RuntimeError{Msg: err.Error(), Span: span}
		}
		return true, val, nil
	}
	if domainNouns[first] {
		return true, shell.Value{}, shell.SpanErrorf(span, "domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(repl.ArgTexts(args), " "))
	}
	return false, shell.Value{}, nil
}

// bpfmanBindFallback is the bind-position fallback the repl
// loop calls when no registered builtin matches. It owns the
// wait and net-exec fast paths (which need the bind's Rc to
// reflect the captured inner outcome), the bpfman dispatch
// bridge, and the "forgot the bpfman prefix" diagnostic.
// Unknown first words fall through to external execution.
func bpfmanBindFallback(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, args []shell.Arg, loc repl.SourceLoc, span shell.Span) (bool, shell.BindResult, error) {
	if len(args) == 0 {
		return false, shell.BindResult{}, nil
	}
	first := repl.ArgText(args[0])

	// 'wait $job' is special-cased so the bind's Rc reflects
	// the JOB's outcome, not merely "wait succeeded".
	if first == "wait" {
		envEnv, err := replWait(ctx, args[1:])
		if err != nil {
			return true, shell.BindResult{}, err
		}
		return true, shell.BindResult{Rc: envEnv, Primary: shell.ValueFromEnvelope(envEnv)}, nil
	}

	// 'net exec $pair CMD...' captures into a real envelope
	// so the bind's Rc reflects the netns command's actual
	// outcome.
	if first == "net" && len(args) >= 2 && repl.ArgText(args[1]) == "exec" {
		envEnv, err := replNetExec(ctx, args[2:])
		if err != nil {
			return true, shell.BindResult{}, err
		}
		return true, shell.BindResult{Rc: envEnv, Primary: shell.ValueFromEnvelope(envEnv)}, nil
	}

	if first == "bpfman" {
		quiet := cli.WithDiscardOutput()
		val, err := replDispatch(ctx, quiet, mgr, args)
		if err != nil {
			rc := shell.Envelope{OK: false, Code: 1, Stderr: err.Error()}
			return true, shell.BindResult{Rc: rc, Primary: shell.Value{}}, nil
		}
		rc := shell.Envelope{OK: true, Code: 0}
		return true, shell.BindResult{Rc: rc, Primary: val}, nil
	}
	if domainNouns[first] {
		rc := shell.Envelope{
			OK:     false,
			Code:   1,
			Stderr: fmt.Sprintf("domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(repl.ArgTexts(args), " ")),
		}
		return true, shell.BindResult{Rc: rc, Primary: shell.ValueFromEnvelope(rc)}, nil
	}
	return false, shell.BindResult{}, nil
}

// replDispatch routes a "bpfman ..." command to the active
// backend. The library backend invokes the in-process Go API
// against the supplied CLI and Manager; the external backend
// forks the bpfman binary as a subprocess and decodes its JSON
// output. The toggle is BPFMAN_DISPATCH (library | external),
// read once at process start; cli and mgr are unused under the
// external backend.
func replDispatch(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []shell.Arg) (shell.Value, error) {
	if bpfmanDispatchMode == dispatchExternal {
		return replDispatchExternal(ctx, args)
	}
	return replDispatchLibrary(ctx, cli, mgr, args)
}

// replDispatchLibrary dispatches a "bpfman ..." command into the
// in-process domain pipeline. parseCommand routes arguments to
// the per-command parser and returns a typed Command node;
// execCommand dispatches via a type-switch.
func replDispatchLibrary(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 || repl.ArgText(args[0]) != "bpfman" {
		return shell.Value{}, fmt.Errorf("expected a command starting with \"bpfman\", got %v", repl.ArgTexts(args))
	}
	cmd, err := parseCommand(args[1:])
	if err != nil {
		return shell.Value{}, err
	}
	if cmd == nil {
		return shell.Value{}, fmt.Errorf("missing command after \"bpfman\"; try \"bpfman program list\"")
	}
	return execCommand(ctx, cli, mgr, cmd)
}

// handleHelp renders the overview (no args) or the detail for
// one named builtin or keyword (one arg).
func handleHelp(c builtinCtx) (shell.Value, error) {
	args := repl.ArgTexts(c.Args)
	switch len(args) {
	case 0:
		return shell.Value{}, c.CLI.PrintOut(renderHelpOverview())
	case 1:
		out, ok := renderHelpDetail(args[0])
		if !ok {
			return shell.Value{}, fmt.Errorf("no help for %q (try 'help' for the overview)", args[0])
		}
		return shell.Value{}, c.CLI.PrintOut(out)
	default:
		return shell.Value{}, fmt.Errorf("help takes at most one argument")
	}
}

// renderHelpOverview composes the no-arg help. The 'Domain
// commands' block stays hand-curated because the bpfman
// subcommand grammar is multi-level and does not fit a flat
// registry; everything else is registry-derived.
func renderHelpOverview() string {
	var b strings.Builder
	b.WriteString("Available commands:\n\n")
	b.WriteString(domainCommandsBlock)
	for _, cat := range categoryOrder {
		writeBuiltinCategory(&b, cat)
	}
	writeKeywordSection(&b)
	b.WriteString("'help <name>' shows the long-form help for a builtin or keyword.\n")
	return b.String()
}

// domainCommandsBlock is the hand-curated top section of the
// help overview.
const domainCommandsBlock = `Domain commands (require "bpfman" prefix):

  Program management:
    bpfman program list [flags]                     List managed BPF programs
    bpfman program get <id>                         Get program details (assignable)
    bpfman program load file [flags]                Load from a local object file (assignable)
    bpfman program load image [flags]               Load from an OCI image (assignable)
    bpfman program unload <ids>                     Unload programs
    bpfman program delete (<ids> | --all) [-r]      Delete with cascading cleanup
    bpfman show program <id> [view] [-o]            Inspect (views: links, maps, paths)

  Link management:
    bpfman link attach <type> [flags] <id>          Attach a program (assignable)
    bpfman link detach <link-ids>                   Detach links
    bpfman link get <link-id>                       Get link details (assignable)
    bpfman link list [flags]                        List managed links
    bpfman link delete <link-ids> [-r]              Delete with cascading cleanup

  Dispatcher management:
    bpfman dispatcher list [--type <type>]           List dispatchers
    bpfman dispatcher get <type> <nsid> <ifindex>    Get dispatcher details
    bpfman dispatcher delete <type> <nsid> <ifindex> Delete a dispatcher

  Diagnostics:
    bpfman audit [rules]                            Audit coherency (read-only)
    bpfman audit explain [rule]                     Explain a coherency rule

`

const helpUsageWrapWidth = 48
const helpRowIndent = "  "

// writeAlignedRows emits Usage / Summary rows with column-aligned
// spacing computed dynamically by text/tabwriter.
func writeAlignedRows(b *strings.Builder, rows [][2]string) {
	tw := newHelpTabwriter(b)
	for _, row := range rows {
		usage, summary := row[0], row[1]
		if len(usage) > helpUsageWrapWidth {
			_ = tw.Flush()
			fmt.Fprintf(b, "%s%s\n%s    %s\n", helpRowIndent, usage, helpRowIndent, summary)
			tw = newHelpTabwriter(b)
			continue
		}
		fmt.Fprintf(tw, "%s%s\t%s\n", helpRowIndent, usage, summary)
	}
	_ = tw.Flush()
}

// newHelpTabwriter constructs the tabwriter used by every help section.
func newHelpTabwriter(b *strings.Builder) *tabwriter.Writer {
	return tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
}

// writeBuiltinCategory emits one section of the overview.
func writeBuiltinCategory(b *strings.Builder, cat string) {
	var entries []builtin
	for _, bi := range repl.Builtins() {
		if bi.Category == cat {
			entries = append(entries, bi)
		}
	}
	if len(entries) == 0 {
		return
	}
	slices.SortFunc(entries, func(a, b builtin) int { return cmp.Compare(a.Name, b.Name) })
	fmt.Fprintf(b, "%s:\n", categoryLabels[cat])
	rows := make([][2]string, 0, len(entries))
	for _, bi := range entries {
		rows = append(rows, [2]string{bi.Usage, bi.Summary})
	}
	writeAlignedRows(b, rows)
	b.WriteString("\n")
}

// writeKeywordSection emits the parser-level keyword section.
func writeKeywordSection(b *strings.Builder) {
	if len(repl.Keywords()) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(repl.Keywords()))
	b.WriteString("Keywords:\n")
	rows := make([][2]string, 0, len(names))
	for _, n := range names {
		k := repl.Keywords()[n]
		rows = append(rows, [2]string{k.Usage, k.Summary})
	}
	writeAlignedRows(b, rows)
	b.WriteString("\n")
}

// renderHelpDetail looks name up in the builtin registry, then
// the keyword registry, and renders Usage / Summary / Detail.
func renderHelpDetail(name string) (string, bool) {
	if bi, ok := repl.Builtins()[name]; ok {
		return formatDetail(bi.Name, bi.Usage, bi.Summary, bi.Detail), true
	}
	if kw, ok := repl.Keywords()[name]; ok {
		return formatDetail(kw.Name, kw.Usage, kw.Summary, kw.Detail), true
	}
	return "", false
}

// formatDetail renders the detail block for one entry.
func formatDetail(name, usage, summary, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", name)
	if usage != "" {
		fmt.Fprintf(&b, "  usage: %s\n", usage)
	}
	if summary != "" {
		fmt.Fprintf(&b, "  %s\n", summary)
	}
	if detail != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}

// replHistoryPath returns the path to the REPL history file,
// following the XDG Base Directory specification.
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

// handleVersion prints version information.
func handleVersion(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, c.CLI.PrintOut(version.Get().Long())
}

// replLoop is a thin test seam wrapping repl.Loop with the
// bpfman-side fallbacks pre-wired. The full bpfman-shell binary
// goes through Run; tests that want to drive the loop directly
// over a string source call this wrapper.
func replLoop(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr repl.LineReader, session *shell.Session, file string, interactive, noCheck bool) error {
	return repl.Loop(ctx, repl.Config{
		CLI:            cli,
		Mgr:            mgr,
		LineReader:     lr,
		Session:        session,
		File:           file,
		Interactive:    interactive,
		NoCheck:        noCheck,
		Fallback:       bpfmanFallback,
		BindFallback:   bpfmanBindFallback,
		MakeAssertStmt: makeExecAssertStmt,
		PromptPrimary:  bpfmanShellPromptPrimary,
		PromptContinue: bpfmanShellPromptContinue,
	})
}
