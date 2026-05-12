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
	"slices"
	"strings"

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

	// Pos is the source location of the chunk this builtin
	// was dispatched from. Used by 'assert'/'require' for
	// chunk-line composition on failure messages and by
	// 'start' to carry the start-site origin into the Job
	// handle.
	Pos sourceLoc

	// Span is the source extent of the originating CommandStmt
	// or BindStmt. Handlers that emit *shell.SyntaxError use it
	// to frame diagnostics at the failing command rather than
	// at the chunk start. Set by replShellCmd from the value
	// the evaluator threaded through Env.ExecCommand /
	// Env.ExecBind.
	Span shell.Span
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

// Category constants group builtins in the help overview.
// Constants rather than strings so a typo in one entry shows
// up at compile time rather than scattering an entry into a
// new section.
const (
	categorySession = "session" // bindings, defs, aliases
	categoryIO      = "io"      // external commands, file, jq, print, source
	categoryAssert  = "assert"  // assert / require
	categoryJobs    = "jobs"    // start / wait / kill / jobs
	categoryMeta    = "meta"    // help / version
)

// categoryLabels maps a category constant to the display label
// used in the help overview. categoryOrder fixes the rendering
// sequence so the overview reads the same on every run; map
// iteration order would otherwise produce arbitrary output.
var categoryLabels = map[string]string{
	categorySession: "Session and bindings",
	categoryIO:      "I/O and external commands",
	categoryJobs:    "Async jobs",
	categoryAssert:  "Assertions",
	categoryMeta:    "Meta",
}

var categoryOrder = []string{
	categorySession,
	categoryIO,
	categoryJobs,
	categoryAssert,
	categoryMeta,
}

// builtin describes one entry in the registry: how to run it,
// how to complete its arguments, what shape its bind-RHS primary
// produces at static-check time, and how to document itself. The
// doc fields drive 'help' (overview) and 'help <name>' (detail).
// Empty fields degrade gracefully -- a builtin with no Detail just
// shows Usage+Summary on detail lookup, and a nil BindShape falls
// through to the inferBindShape default (an external-subprocess
// result envelope). Pointer-free because the registry is read-only
// at runtime.
type builtin struct {
	Name     string
	Handler  func(builtinCtx) (shell.Value, error)
	Complete argCompleter // nil = generic fallthrough

	// BindShape lets the static checker resolve `<- NAME ARGS...`
	// to the correct primary Shape. The function receives the
	// arguments after the command name so subcommand-aware shapes
	// (`net veth-pair` -> NetPair, `net release` -> result, ...)
	// live next to the handler that produces them. Wrap a fixed
	// Shape with shell.StaticBindShape; nil leaves the checker on
	// the default external-subprocess result envelope.
	BindShape shell.BindShapeFn

	Category string // categoryXxx constant; ungrouped if empty
	Usage    string // one-line syntax (e.g. "kill [--signal=NAME] [--grace=DUR] $job")
	Summary  string // one-line description shown next to Usage
	Detail   string // multi-paragraph long help; optional
}

// keyword describes a parser-level form (let, guard, defer,
// def, bpfman) that participates in 'help' but is not part of
// the dispatch table. Keywords have no handler and no per-call
// completer because they are recognised by the parser before
// dispatch ever runs.
type keyword struct {
	Name    string
	Usage   string
	Summary string
	Detail  string
}

// keywordRegistry is the documentation source of truth for
// parser-level forms. 'help' renders these in their own
// section and 'help <name>' looks them up the same way it
// looks up builtins. Adding a keyword: one entry here; the
// help and completion paths pick it up automatically.
var keywordRegistry = map[string]keyword{
	"let": {
		Name:    "let",
		Usage:   "let X = EXPR  |  let X <- COMMAND  |  let (rc, X) <- COMMAND",
		Summary: "Bind an expression result, a command's primary, or a (rc, primary) pair.",
		Detail: "let evaluates the right-hand side and binds the named variable(s) " +
			"in the current session. The '<-' form runs a command and binds its " +
			"primary result; failure flows into the variable as a result with " +
			"ok=false rather than halting the script. Use 'guard' for the " +
			"halt-on-failure variant.",
	},
	"guard": {
		Name:    "guard",
		Usage:   "guard X <- COMMAND  |  guard (rc, X) <- COMMAND",
		Summary: "Bind primary (and optionally rc); halt the script on a non-ok rc.",
		Detail: "guard is the let-with-halt-on-failure form. If the captured rc is " +
			"not ok, the script aborts and the driver renders the failed command's " +
			"location, argv, and stderr. Use this when the next statement only " +
			"makes sense if the command succeeded.",
	},
	"defer": {
		Name:    "defer",
		Usage:   "defer COMMAND ARGS",
		Summary: "Run COMMAND when the enclosing defer scope exits.",
		Detail: "Arguments are evaluated at register time so the captured values " +
			"do not change if the variables they reference are later reassigned. " +
			"Defers fire in LIFO order at scope exit. The enclosing scope is the " +
			"script in script mode, the sourced file inside source, the prompt " +
			"chunk in interactive mode, and the def body inside a def. " +
			"'defer kill $p' is the canonical async-job cleanup idiom. The killed " +
			"job stays in the 'jobs' ledger until an explicit 'reap'.",
	},
	"def": {
		Name:    "def",
		Usage:   "def NAME(P1, P2, ...) { BODY }",
		Summary: "Define a user command callable as 'NAME ARG1 ARG2 ...'.",
		Detail: "The body opens its own defer scope so a 'defer cleanup' inside the " +
			"def fires when the def returns. Jobs started inside a def join the " +
			"caller's job scope (so returning a $job handle for the caller to wait " +
			"on does not register as a leak).",
	},
	"bpfman": {
		Name:    "bpfman",
		Usage:   "bpfman <subcommand> ...",
		Summary: "Domain-command namespace prefix for program / link / dispatcher / audit verbs.",
		Detail: "All bpfman domain operations live behind this prefix to keep them " +
			"distinct from shell builtins and aliases. See the 'Domain commands' " +
			"section of the overview for the available subcommands.",
	},
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
		"alias": {
			Name: "alias", Handler: handleAlias,
			Category: categorySession,
			Usage:    "alias <name> = <expansion>",
			Summary:  "Define a first-token alias.",
		},
		"aliases": {
			Name: "aliases", Handler: handleAliases,
			Category: categorySession,
			Usage:    "aliases",
			Summary:  "List defined aliases.",
		},
		"assert": {
			Name: "assert", Handler: handleAssert,
			Category: categoryAssert,
			Usage:    "assert <verb> [args...]  |  assert <bool-expr>  |  assert not <verb> [args...]",
			Summary:  "Check a condition; continue on failure but record an assertion-failure for the exit code.",
			Detail: "Verbs: nil, not-empty, ok, fail, path exists, contains, matches. " +
				"Infix operators: == != < <= > >= (semantics chosen by operand type). " +
				"The single-arg form takes any boolean expression: 'assert $flag', " +
				"'assert true'. Use 'require' for halt-on-failure semantics. Coerce " +
				"stringy numeric input via [$x |> jq tonumber] before comparing.",
		},
		"defs": {
			Name: "defs", Handler: handleDefs,
			Category: categorySession,
			Usage:    "defs",
			Summary:  "List user-defined commands.",
		},
		"exec": {
			Name: "exec", Handler: handleExec,
			BindShape: shell.StaticBindShape(shell.KindShape(shell.OriginEnvelope)),
			Category:  categoryIO,
			Usage:     "exec <command> [args | file:$var]...",
			Summary:   "Run a host command. Use 'file:$var' to materialise a structured value as a temp file.",
		},
		"file": {
			Name: "file", Handler: handleFile,
			BindShape: shell.StaticBindShape(shell.Shape{Sealed: false, Kind: shell.OriginUnknown}),
			Category:  categoryIO,
			Usage:     "file temp $var[.path]",
			Summary:   "Write a value to a temp file; primary is the path (assignable).",
		},
		"help": {
			Name: "help", Handler: handleHelp, Complete: completeHelpArg,
			Category: categoryMeta,
			Usage:    "help [<name>]",
			Summary:  "Show the overview, or detailed help for a specific builtin or keyword.",
			Detail: "With no arguments, render the overview grouped by category. " +
				"With one argument, look the name up in the builtin and keyword " +
				"registries and print Usage, Summary, and the long-form Detail " +
				"if any.",
		},
		"jobs": {
			Name: "jobs", Handler: handleJobs,
			Category: categoryJobs,
			Usage:    "jobs",
			Summary:  "List jobs registered in the current scope.",
			Detail: "Read-only: peeking at status does not mark any job Managed. Status " +
				"buckets are running, killing (kill issued, reaper has not yet " +
				"observed exit), exited N, killed SIG.",
		},
		"jq": {
			Name: "jq", Handler: handleJQ,
			Category: categoryIO,
			Usage:    "jq <filter> <value>",
			Summary:  "Apply a jq filter to a value (assignable).",
		},
		"kill": {
			Name: "kill", Handler: handleKill,
			BindShape: shell.StaticBindShape(shell.KindShape(shell.OriginEnvelope)),
			Category:  categoryJobs,
			Usage:     "kill [--signal=NAME] [--grace=DUR] $job",
			Summary:   "Terminate a job. Default: SIGTERM, 2s grace, SIGKILL if still alive; blocks until reaped.",
			Detail: "The default path sends SIGTERM, waits up to --grace (default 2s), " +
				"escalates to SIGKILL if the process is still alive, and blocks " +
				"until the reaper has settled. --grace=0 sends SIGTERM and SIGKILL " +
				"back-to-back. --signal=NAME (e.g. USR1, HUP) delivers a custom " +
				"signal and returns immediately without escalation; use this for " +
				"control-flow signals, not for termination. " +
				"kill marks the job as managed but leaves the entry in the ledger; " +
				"the killed status is observable in 'jobs' until 'reap' drops it. " +
				"'defer kill $p' is the canonical async cleanup idiom.",
		},
		"print": {
			Name: "print", Handler: handlePrint, Complete: completePrintArg,
			Category: categoryIO,
			Usage:    "print <value>...",
			Summary:  "Print one or more values (one pretty, many compact space-joined).",
		},
		"reap": {
			Name: "reap", Handler: handleReap,
			Category: categoryJobs,
			Usage:    "reap",
			Summary:  "Drop completed jobs from the registry; running jobs are left alone.",
			Detail: "Always explicit: nothing reaps automatically when wait or kill " +
				"returns, because the script may still want to inspect $job after " +
				"the call. After 'reap', the 'jobs' listing reflects only entries " +
				"whose process is still running.",
		},
		"require": {
			Name: "require", Handler: handleRequire,
			Category: categoryAssert,
			Usage:    "require <verb> [args...]  |  require <bool-expr>  |  require not <verb> [args...]",
			Summary:  "Check a condition; halt the script on failure.",
			Detail: "Verbs and operators are the same as assert. Use require where the " +
				"following statements only make sense if the condition holds.",
		},
		"source": {
			Name: "source", Handler: handleSource, Complete: completeSourceArg,
			Category: categoryIO,
			Usage:    "source <file>",
			Summary:  "Execute commands from a file in the caller's session.",
			Detail: "Variables, defs, aliases, and jobs started in the file inherit the " +
				"caller's scope. Defers in the file fire when source returns " +
				"(file-as-cleanup-unit). Nested source is rejected.",
		},
		"tempdir": {
			Name: "tempdir", Handler: handleTempdir,
			BindShape: shell.StaticBindShape(shell.Shape{Sealed: false, Kind: shell.OriginUnknown}),
			Category:  categoryIO,
			Usage:     "tempdir <prefix>",
			Summary:   "Create a private temp directory; primary carries .path (assignable).",
			Detail: "Wraps os.MkdirTemp under the OS default temp dir. <prefix> names " +
				"the leading component (so concurrent runs are distinguishable in " +
				"ls /tmp); the random suffix guarantees uniqueness. Cleanup is the " +
				"caller's responsibility -- pair with 'defer rm -rf $wd.path' for " +
				"the canonical lifecycle. Use this in place of hard-coded /tmp " +
				"paths whenever a script may run concurrently with itself, since " +
				"shared paths race on rm/touch operations across instances.",
		},
		"start": {
			Name: "start", Handler: handleStart,
			BindShape: shell.StaticBindShape(shell.KindShape(shell.OriginJob)),
			Category:  categoryJobs,
			Usage:     "start <command> [args]",
			Summary:   "Spawn a background process; primary is a $job handle (assignable).",
			Detail: "The job runs as a process-group leader so 'kill' reaches every " +
				"descendant. Output is captured into the handle's Stdout/Stderr; " +
				"the script reads them after 'wait' returns. " +
				"start registers the job in the active scope's ledger; the entry " +
				"persists through wait, kill, and beyond until an explicit 'reap' " +
				"or the scope's leak handler at unwind. Script mode treats an " +
				"unwaited/unkilled job as a leak (FAIL, exit 1); interactive mode " +
				"silently SIGKILLs on session exit.",
		},
		"fire": {
			Name: "fire", Handler: handleFire,
			BindShape: shell.StaticBindShape(shell.KindShape(shell.OriginJob)),
			Category:  categoryJobs,
			Usage:     "fire <kind> <sentinel> <ack> --count=N [--waves=K]",
			Summary:   "Spawn a deterministic kernel-stimulus worker; primary is a $job handle (assignable).",
			Detail: "fire is a typed wrapper over start for e2e fixtures. <kind> selects " +
				"one of the registered kernel-event generators (unlinkat, kill, uprobe). " +
				"sentinel/ack are file-path prefixes for the wave protocol: the worker " +
				"blocks on sentinel.W, fires --count=N events, and creates ack.W per " +
				"wave W in 1..K. --waves defaults to 1. Uprobe kinds publish " +
				"target_binary on the returned $job so 'bpfman link attach uprobe " +
				"--target $work.target_binary' attaches to the running bpfman-shell ELF. " +
				"start env BPFMAN_SHELL_MODE=... remains valid as a debug escape hatch.",
		},
		"net": {
			Name: "net", Handler: handleNet,
			BindShape: netBindShape,
			Category:  categoryJobs,
			Usage: "net veth-pair --ns=NS --host-link=NAME --host-addr=CIDR --peer-link=NAME --peer-addr=CIDR [--no-routes]  |  " +
				"net release $pair  |  net exec $pair CMD ARGS...  |  net start $pair CMD ARGS...",
			Summary: "Paired-veth single-netns topology fixture for TC / TCX / XDP dispatcher tests.",
			Detail: "net is the e2e built-in for the topology dispatcher tests share: a single " +
				"veth pair, a netns the peer end lives in, two /32 addresses, and the two " +
				"symmetric routes that make the pair pingable. veth-pair builds the whole " +
				"thing in one call and returns a $pair handle whose fields (ns, host_link, " +
				"peer_link, host_addr, peer_addr) thread through 'bpfman link attach -i " +
				"$pair.host_link' and 'net exec $pair ping $pair.peer_addr'. release tears " +
				"the topology down in LIFO order and is idempotent (re-release is a no-op; " +
				"missing resources are fine). exec runs a command in the netns and captures " +
				"the result envelope (sync); start launches a command in the netns as a " +
				"background $job (async). Operational use after release is a runtime error; " +
				"field reads stay valid. Raw ip(8) remains the documented escape hatch for " +
				"topologies net does not cover (bridges, VLANs, IPv6, multiple pairs).",
		},
		"unalias": {
			Name: "unalias", Handler: handleUnalias,
			Category: categorySession,
			Usage:    "unalias <name>...",
			Summary:  "Remove alias bindings.",
		},
		"undef": {
			Name: "undef", Handler: handleUndef,
			Category: categorySession,
			Usage:    "undef <name>...",
			Summary:  "Remove user-defined commands.",
		},
		"range": {
			Name: "range", Handler: handleRange,
			Category: categoryIO,
			Usage:    "range <integer>",
			Summary:  "Produce the sequence [0, 1, ..., N-1] (jq-style range; assignable).",
			Detail: "Mirrors jq's 'range(N)' semantics: zero-indexed, upper bound " +
				"exclusive. range is a pure builtin and is called from expression " +
				"position only: 'foreach i in (range 5) { ... }' or 'let xs = range 5'. " +
				"The '<-' binding form is rejected because pure builtins produce no " +
				"result envelope. Negative bounds are rejected; the upper limit is " +
				"INT32_MAX to keep pathological scripts loud rather than OOM.",
		},
		"u32le": {
			Name: "u32le", Handler: handleU32LE,
			Category: categoryIO,
			Usage:    "u32le <integer>",
			Summary:  "Encode an integer as a 4-byte little-endian hex string.",
			Detail: "Returns 8 lowercase hex characters with no 0x prefix. " +
				"Useful for `bpfman -g NAME=HEX` global-data injection " +
				"where the .bpf.c declares `volatile const __u32`. " +
				"Rejects negative inputs and values that exceed UINT32_MAX.",
		},
		"u64le": {
			Name: "u64le", Handler: handleU64LE,
			Category: categoryIO,
			Usage:    "u64le <integer>",
			Summary:  "Encode an integer as an 8-byte little-endian hex string.",
			Detail: "Returns 16 lowercase hex characters with no 0x prefix. " +
				"Useful for `bpfman -g NAME=HEX` global-data injection " +
				"where the .bpf.c declares `volatile const __u64`. " +
				"Rejects negative inputs (Go uint64 max is the upper bound).",
		},
		"unset": {
			Name: "unset", Handler: handleUnset, Complete: completeUnsetArg,
			Category: categorySession,
			Usage:    "unset <name>...",
			Summary:  "Remove variable bindings.",
		},
		"vars": {
			Name: "vars", Handler: handleVars,
			Category: categorySession,
			Usage:    "vars",
			Summary:  "List session variables and their kinds.",
		},
		"version": {
			Name: "version", Handler: handleVersion,
			Category: categoryMeta,
			Usage:    "version",
			Summary:  "Print version information.",
		},
		"wait": {
			Name: "wait", Handler: handleWait,
			BindShape: shell.StaticBindShape(shell.KindShape(shell.OriginEnvelope)),
			Category:  categoryJobs,
			Usage:     "wait $job",
			Summary:   "Block until the job exits; primary is the captured result (assignable).",
			Detail: "The result carries ok, code, stdout, stderr, killed, signal. " +
				"A killed job that the script asked to terminate reports killed=true " +
				"with signal set; the script distinguishes 'I asked for this' from " +
				"'real failure' via $r.killed rather than $r.ok. " +
				"wait marks the job as managed but leaves the entry in the ledger so " +
				"$job stays inspectable; use 'reap' to drop completed entries.",
		},
	}

	// Register each effectful builtin's BindShape with the shell
	// package so the static checker (shell/check.go's inferBindShape)
	// resolves `<- NAME ARGS...` to the correct primary Shape
	// without a hand-maintained switch. Entries with a nil BindShape
	// (alias, vars, help, jobs, reap, ...) bind nothing assignable
	// and so contribute no shape; inferBindShape's default takes
	// care of the rest (external-subprocess result).
	for name, b := range builtinRegistry {
		if b.BindShape != nil {
			shell.RegisterBindShape(name, b.BindShape)
		}
	}
}

// handleAssert runs replAssertRequire in non-halting mode. The
// shared body distinguishes assert from require by an internal
// bool; the registry keeps them as two entries pointing at
// dedicated wrappers so each call site is grep-able by verb.
// Any error from the verb dispatcher is framed at the command's
// Span by the dispatcher in replShellCmd; handler-level
// wrapping is unnecessary.
func handleAssert(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replAssertRequire(c.Ctx, c.CLI, c.Mgr, c.Env.Session, c.Args, false, c.Pos)
}

// handleRequire runs replAssertRequire in halt-on-fail mode.
func handleRequire(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replAssertRequire(c.Ctx, c.CLI, c.Mgr, c.Env.Session, c.Args, true, c.Pos)
}

// handleExec adapts replExec to the builtin shape.
func handleExec(c builtinCtx) (shell.Value, error) {
	return replExec(c.Ctx, c.CLI, c.Args)
}

// handleFile adapts replFile to the builtin shape.
func handleFile(c builtinCtx) (shell.Value, error) {
	return replFile(c.CLI, c.Args)
}

// handleHelp adapts replHelp to the builtin shape, plumbing
// the user's argument (if any) through so 'help <name>' looks
// up the named builtin or keyword.
func handleHelp(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, replHelp(c.CLI, argTexts(c.Args))
}

// handleJobs lists jobs registered in the active job scope.
// Rejects extra arguments at the dispatch boundary so the
// error message is consistent with the other arg-less builtins.
func handleJobs(c builtinCtx) (shell.Value, error) {
	if len(c.Args) > 0 {
		return shell.Value{}, shell.SpanErrorf(c.Span, "jobs takes no arguments")
	}
	return shell.Value{}, replJobs(c.CLI, c.Env)
}

// handleReap drops completed jobs from the active scope's
// registry. Pure mutation; the caller has already typed
// 'jobs' to see what is there and now wants the listing
// trimmed.
func handleReap(c builtinCtx) (shell.Value, error) {
	if len(c.Args) > 0 {
		return shell.Value{}, shell.SpanErrorf(c.Span, "reap takes no arguments")
	}
	return shell.Value{}, replReap(c.Env)
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

// handleSource adapts replSource to the builtin shape. The
// nil-env guard catches embedders that forget to plumb env
// through; the dispatcher already enforces this before calling.
func handleSource(c builtinCtx) (shell.Value, error) {
	if c.Env == nil {
		return shell.Value{}, shell.SpanErrorf(c.Span, "source requires an active shell environment")
	}
	return shell.Value{}, replSource(c.Ctx, c.CLI, c.Mgr, c.Env, argTexts(c.Args))
}

// handleStart adapts replStart to the builtin shape. Origin is
// derived from the chunk's loc so the leak-walk diagnostic can
// cite the start site even when the leak fires far from it.
func handleStart(c builtinCtx) (shell.Value, error) {
	return replStart(c.Ctx, c.Env, c.Pos.cite(), c.Args)
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

// completeHelpArg offers builtin and keyword names for 'help
// <name>'. Walks both registries so a 'help def<TAB>' pulls in
// def, defs, defer at once.
func completeHelpArg(session *shell.Session, baseDir string, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	_ = session
	_ = baseDir
	prefix := ""
	if len(tokens) >= 2 && !trailingSpace {
		prefix = tokens[len(tokens)-1]
	}
	var names []string
	for n := range builtinRegistry {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n+" ")
		}
	}
	for n := range keywordRegistry {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n+" ")
		}
	}
	slices.Sort(names)
	return names, len(prefix)
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
