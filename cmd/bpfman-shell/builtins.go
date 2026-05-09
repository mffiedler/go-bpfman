// Shell builtin registry: one source of truth for the set of
// shell-language commands (alias, defer, jobs, kill, source,
// ...). Adding a new builtin is a single map entry: the
// dispatcher reads from the registry, the completer derives
// its first-token candidates from the registry, and per-builtin
// argument completion (file paths for 'source', $-led variable
// references for 'print', and so on) travels with each entry.
//
// Domain commands ("bpfman program load ...") are deliberately
// out of scope here. They live in a different namespace
// (parseCommand under the leading "bpfman" prefix), follow a
// multi-level subcommand grammar that does not fit a flat
// first-token registry, and have their own completion path
// (replCompleteBpfman). Mixing them into one registry would
// create a heterogeneous lookup with two different shapes; the
// split keeps each registry small and obvious.
package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/shell"
)

// builtinCtx carries everything a handler might need. It is a
// fat context by design: the dependency superset across the
// 19 dispatched builtins is small (ctx, cli, mgr, env, args,
// loc) and uniform plumbing keeps each handler's wrapper to a
// few lines. Session is reachable via Env.Session; isRequire
// and origin-style toggles are handler-internal and need no
// dedicated field.
type builtinCtx struct {
	// Ctx is the cancellation/deadline context for the call.
	// Plumbed from replShellCmd's caller; long-running
	// builtins (start, wait, kill's escalation wait, exec)
	// observe it.
	Ctx context.Context

	// CLI carries the stdout/stderr writers and the
	// PrintOut/PrintErrf helpers.
	CLI *bpfmancli.CLI

	// Mgr is the bpfman manager. Only the assertion verbs
	// and source touch it; most builtins leave it nil-safe.
	Mgr *manager.Manager

	// Env is the active shell environment. Env.Session is
	// the canonical handle to variable bindings, defs, and
	// aliases; jobs and source read Env directly to register
	// background processes and to inherit the caller's
	// scope.
	Env *shell.Env

	// Cmd is the command name as the user typed it (args[0]
	// before slicing). Useful for diagnostic messages that
	// quote the command spelling.
	Cmd string

	// Args is the argument list with the command name
	// already stripped. Most handlers use it directly; a
	// few that take []string convert via argTexts(c.Args).
	Args []shell.Arg

	// Loc is the source location of the chunk this builtin
	// was dispatched from. Used by 'assert'/'require' for
	// chunk-line composition on failure messages and by
	// 'start' to carry the start-site origin into the Job
	// handle.
	Loc sourceLoc
}

// argCompleter is a per-builtin completion callback for tokens
// after the command name. tokens[0] is the command itself;
// tokens[1:] are the user-typed arguments. trailingSpace says
// whether the cursor is positioned just after a space (so a
// new token is starting) or in the middle of a partial token
// (so the last entry of tokens is being typed). baseDir is
// the working directory for filesystem completions; it is the
// empty string in production (relative to the process cwd) and
// a fixture path in tests, threaded through from
// replCompleteIn.
//
// Returning candidates with replace == 0 means "the candidates
// are independent of the partial token" (e.g. an empty list of
// completions or a fixed menu). Returning replace == len(prefix)
// signals "rewrite the last len(prefix) characters with the
// chosen candidate".
type argCompleter func(session *shell.Session, baseDir string, tokens []string, trailingSpace bool) (candidates []string, replace int)

// builtin describes one entry in the registry: how to run it
// and how to complete its arguments. Pointer-free because the
// registry is read-only at runtime; new builtins are added at
// init time and the map is never mutated thereafter.
type builtin struct {
	Name     string
	Handler  func(builtinCtx) (shell.Value, error)
	Complete argCompleter // nil = generic fallthrough
}

// builtinRegistry is the single source of truth for the shell
// builtin set. The dispatcher (replShellCmd) does table lookup;
// the completer's first-token candidates derive from this map;
// the alias check in replAlias guards against shadowing by
// looking up the candidate name here. Adding a new builtin is
// one entry.
//
// Populated in init() rather than at the top level so the
// handler references do not form a compile-time initialisation
// cycle: replAssertRequire (used by handleAssert / handleRequire)
// transitively reaches replShellCmd via runCommand, and
// replShellCmd reads builtinRegistry. Deferring construction
// to init() breaks the cycle while keeping the map read-only
// at runtime.
var builtinRegistry map[string]builtin

func init() {
	builtinRegistry = map[string]builtin{
		"alias":   {Name: "alias", Handler: handleAlias},
		"aliases": {Name: "aliases", Handler: handleAliases},
		"assert":  {Name: "assert", Handler: handleAssert},
		"defs":    {Name: "defs", Handler: handleDefs},
		"exec":    {Name: "exec", Handler: handleExec},
		"file":    {Name: "file", Handler: handleFile},
		"help":    {Name: "help", Handler: handleHelp},
		"jobs":    {Name: "jobs", Handler: handleJobs},
		"jq":      {Name: "jq", Handler: handleJQ},
		"kill":    {Name: "kill", Handler: handleKill},
		"print":   {Name: "print", Handler: handlePrint, Complete: completePrintArg},
		"require": {Name: "require", Handler: handleRequire},
		"source":  {Name: "source", Handler: handleSource, Complete: completeSourceArg},
		"start":   {Name: "start", Handler: handleStart},
		"unalias": {Name: "unalias", Handler: handleUnalias},
		"undef":   {Name: "undef", Handler: handleUndef},
		"unset":   {Name: "unset", Handler: handleUnset, Complete: completeUnsetArg},
		"vars":    {Name: "vars", Handler: handleVars},
		"version": {Name: "version", Handler: handleVersion},
		"wait":    {Name: "wait", Handler: handleWait},
	}
}

// handleAlias adapts replAlias to the builtin shape.
func handleAlias(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replAlias(c.CLI, c.Env.Session, argTexts(c.Args))
}

// handleAliases adapts replAliases to the builtin shape.
func handleAliases(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replAliases(c.CLI, c.Env.Session)
}

// handleAssert runs replAssertRequire in non-halting mode. The
// shared body distinguishes assert from require by an internal
// bool; the registry keeps them as two entries pointing at
// dedicated wrappers so each call site is grep-able by verb.
func handleAssert(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replAssertRequire(c.Ctx, c.CLI, c.Mgr, c.Env.Session, c.Args, false, c.Loc)
}

// handleRequire runs replAssertRequire in halt-on-fail mode.
func handleRequire(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replAssertRequire(c.Ctx, c.CLI, c.Mgr, c.Env.Session, c.Args, true, c.Loc)
}

// handleDefs adapts replDefs to the builtin shape.
func handleDefs(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replDefs(c.CLI, c.Env.Session)
}

// handleExec adapts replExec to the builtin shape.
func handleExec(c builtinCtx) (shell.Value, error) {
	return replExec(c.Ctx, c.CLI, c.Args)
}

// handleFile adapts replFile to the builtin shape.
func handleFile(c builtinCtx) (shell.Value, error) {
	return replFile(c.CLI, c.Args)
}

// handleHelp adapts replHelp to the builtin shape.
func handleHelp(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replHelp(c.CLI)
}

// handleJobs lists jobs registered in the active job scope.
// Rejects extra arguments at the dispatch boundary so the
// error message is consistent with the other arg-less builtins.
func handleJobs(c builtinCtx) (shell.Value, error) {
	if len(c.Args) > 0 {
		return shell.Value{}, fmt.Errorf("jobs takes no arguments")
	}
	return shell.Value{}, replJobs(c.CLI, c.Env)
}

// handleJQ adapts replJQ to the builtin shape.
func handleJQ(c builtinCtx) (shell.Value, error) {
	return replJQ(c.Args)
}

// handleKill adapts replKill to the builtin shape, wrapping the
// returned Envelope as a Value so the bind path ('let r <-
// kill $p') receives a usable primary.
func handleKill(c builtinCtx) (shell.Value, error) {
	env, err := replKill(c.Ctx, c.Args)
	if err != nil {
		return shell.Value{}, err
	}
	return shell.ValueFromEnvelope(env), nil
}

// handlePrint adapts replPrint to the builtin shape.
func handlePrint(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replPrint(c.CLI, c.Args)
}

// handleSource adapts replSource to the builtin shape. The
// nil-env guard catches embedders that forget to plumb env
// through; the dispatcher already enforces this before calling.
func handleSource(c builtinCtx) (shell.Value, error) {
	if c.Env == nil {
		return shell.Value{}, fmt.Errorf("source requires an active shell environment")
	}
	return shell.Value{}, replSource(c.Ctx, c.CLI, c.Mgr, c.Env, argTexts(c.Args))
}

// handleStart adapts replStart to the builtin shape. Origin is
// derived from the chunk's loc so the leak-walk diagnostic can
// cite the start site even when the leak fires far from it.
func handleStart(c builtinCtx) (shell.Value, error) {
	return replStart(c.Ctx, c.Env, c.Loc.cite(), c.Args)
}

// handleUnalias adapts replUnalias to the builtin shape.
func handleUnalias(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replUnalias(c.CLI, c.Env.Session, argTexts(c.Args))
}

// handleUndef adapts replUndef to the builtin shape.
func handleUndef(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replUndef(c.Env.Session, argTexts(c.Args))
}

// handleUnset adapts replUnset to the builtin shape.
func handleUnset(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replUnset(c.CLI, c.Env.Session, argTexts(c.Args))
}

// handleVars adapts replVars to the builtin shape.
func handleVars(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replVars(c.CLI, c.Env.Session)
}

// handleVersion adapts replVersion to the builtin shape.
func handleVersion(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replVersion(c.CLI)
}

// handleWait adapts replWait to the builtin shape, wrapping the
// returned Envelope as a Value so the bind path ('let r <-
// wait $p') receives a usable primary.
func handleWait(c builtinCtx) (shell.Value, error) {
	env, err := replWait(c.Ctx, c.Args)
	if err != nil {
		return shell.Value{}, err
	}
	return shell.ValueFromEnvelope(env), nil
}

// completeSourceArg offers filesystem completions for 'source
// FILE'.
func completeSourceArg(session *shell.Session, baseDir string, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	if len(tokens) == 1 && trailingSpace {
		return replFileCompletions(baseDir, ""), 0
	}
	if len(tokens) >= 2 {
		prefix := ""
		if !trailingSpace {
			prefix = tokens[len(tokens)-1]
		}
		return replFileCompletions(baseDir, prefix), len(prefix)
	}
	return nil, 0
}

// completePrintArg offers variable-path completion for 'print
// $name.field'. Bare-word arguments are literal strings at
// runtime ('print foo' prints 'foo', not the variable foo), so
// the completer only fires when the prefix is sigil-led; an
// empty prefix lists every variable as candidates.
func completePrintArg(session *shell.Session, baseDir string, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	_ = baseDir
	prefix := ""
	if len(tokens) >= 2 && !trailingSpace {
		prefix = tokens[len(tokens)-1]
	}
	if prefix == "" || (len(prefix) > 0 && prefix[0] == '$') {
		return replCompleteVarPath(session, prefix, true)
	}
	return nil, 0
}

// completeUnsetArg offers bare variable-name completion for
// 'unset NAME'. Unset takes the unprefixed names, not sigil-led
// references, so completePrintArg's variable-path walker would
// be wrong here.
func completeUnsetArg(session *shell.Session, baseDir string, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	_ = baseDir
	prefix := ""
	if len(tokens) >= 2 && !trailingSpace {
		prefix = tokens[len(tokens)-1]
	}
	return replCompleteVarNames(session, prefix)
}
