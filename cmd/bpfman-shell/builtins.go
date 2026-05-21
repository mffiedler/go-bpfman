// Shell builtin registry: one source of truth for the set of
// shell-language commands (defer, jobs, kill, source, ...).
// Adding a new builtin is a single map entry: the
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
	"path/filepath"
	"slices"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// Local aliases keep the dispatch types short in this file and
// in the handler files; the canonical declarations live in repl/.
type (
	builtinCtx = repl.Ctx
	builtin    = repl.Builtin
	keyword    = repl.Keyword
)

// Category aliases for the help registry. The constants and
// rendering maps live in repl/; the locals keep the per-entry
// Category field readable.
const (
	categorySession = repl.CategorySession
	categoryIO      = repl.CategoryIO
	categoryAssert  = repl.CategoryAssert
	categoryJobs    = repl.CategoryJobs
	categoryMeta    = repl.CategoryMeta
)

var (
	categoryLabels = repl.CategoryLabels
	categoryOrder  = repl.CategoryOrder
)

// keywordRegistrations is the bpfman-shell documentation source
// of truth for parser-level forms. The init() at the bottom of
// this file feeds each entry into repl.RegisterKeyword.
var keywordRegistrations = []keyword{
	{
		Name:    "let",
		Usage:   "let X = EXPR  |  let (a b ...) = LIST_EXPR  |  let X <- COMMAND  |  let (rc X) <- COMMAND",
		Summary: "Bind an expression result, a destructured list, a command's primary, or a (rc primary) pair.",
		Detail: "let evaluates the right-hand side and binds the named variable(s) " +
			"in the current session. The '<-' form runs a command and binds its " +
			"primary result; failure flows into the variable as a result with " +
			"ok=false rather than halting the script. Use 'guard' for the " +
			"halt-on-failure variant.",
	},
	{
		Name:    "guard",
		Usage:   "guard X <- COMMAND  |  guard (rc X) <- COMMAND",
		Summary: "Bind primary (and optionally rc); halt the script on a non-ok rc.",
		Detail: "guard is the let-with-halt-on-failure form. If the captured rc is " +
			"not ok, the script aborts and the driver renders the failed command's " +
			"location, argv, and stderr. Use this when the next statement only " +
			"makes sense if the command succeeded.",
	},
	{
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
	{
		Name:    "def",
		Usage:   "def NAME(P1 P2 ...) { BODY }",
		Summary: "Define a user command callable as 'NAME ARG1 ARG2 ...'.",
		Detail: "The body opens its own defer scope so a 'defer cleanup' inside the " +
			"def fires when the def returns. Jobs started inside a def join the " +
			"caller's job scope (so returning a $job handle for the caller to wait " +
			"on does not register as a leak).",
	},
	{
		Name:    "bpfman",
		Usage:   "bpfman <subcommand> ...",
		Summary: "Domain-command namespace prefix for program / link / dispatcher / audit verbs.",
		Detail: "All bpfman domain operations live behind this prefix to keep them " +
			"distinct from shell builtins. See the 'Domain commands' " +
			"section of the overview for the available subcommands.",
	},
}

// builtinRegistrations is the bpfman-shell builtin set. The
// init() at the bottom of this file feeds each entry into
// repl.RegisterBuiltin so the dispatcher (replShellCmd) and
// the completer's first-token candidates see the same source
// of truth.
//
// Populated in an init() rather than at the top level so the
// handler references do not form a compile-time initialisation
// cycle: replAssertRequire (used by handleAssert / handleRequire)
// transitively reaches replShellCmd via runCommand, and the
// dispatcher reads repl.Builtins(). Deferring registration to
// init() breaks the cycle while keeping the registry read-only
// at runtime.
func init() {
	for _, k := range keywordRegistrations {
		repl.RegisterKeyword(k)
	}
	for _, b := range []builtin{
		{
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
		{
			Name: "defs", Handler: handleDefs,
			Category: categorySession,
			Usage:    "defs",
			Summary:  "List user-defined commands.",
		},
		{
			Name: "exec", Handler: handleExec,
			BindShape: shell.StaticBindShape(shell.KindShape(shell.OriginEnvelope)),
			Category:  categoryIO,
			Usage:     "exec <command> [args | file:$var]...",
			Summary:   "Run a host command. Use 'file:$var' to materialise a structured value as a temp file.",
		},
		{
			Name: "file", Handler: handleFile,
			BindShape: shell.StaticBindShape(shell.Shape{Sealed: false, Kind: shell.OriginUnknown}),
			Category:  categoryIO,
			Usage:     "file temp $var[.path]",
			Summary:   "Write a value to a temp file; primary is the path (assignable).",
		},
		{
			Name: "help", Handler: handleHelp, Complete: completeHelpArg,
			Category: categoryMeta,
			Usage:    "help [<name>]",
			Summary:  "Show the overview, or detailed help for a specific builtin or keyword.",
			Detail: "With no arguments, render the overview grouped by category. " +
				"With one argument, look the name up in the builtin and keyword " +
				"registries and print Usage, Summary, and the long-form Detail " +
				"if any.",
		},
		{
			Name: "jobs", Handler: handleJobs,
			Category: categoryJobs,
			Usage:    "jobs",
			Summary:  "List jobs registered in the current scope.",
			Detail: "Read-only: peeking at status does not mark any job Managed. Status " +
				"buckets are running, killing (kill issued, reaper has not yet " +
				"observed exit), exited N, killed SIG.",
		},
		{
			Name: "jq", Handler: handleJQ,
			Category: categoryIO,
			Usage:    "jq <filter> <value>",
			Summary:  "Apply a jq filter to a value (assignable).",
		},
		{
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
		{
			Name: "print", Handler: handlePrint, Complete: completePrintArg,
			Category: categoryIO,
			Usage:    "print [value]...",
			Summary:  "Print zero or more values (none emits a blank line; one pretty; many compact space-joined).",
		},
		{
			Name: "reap", Handler: handleReap,
			Category: categoryJobs,
			Usage:    "reap",
			Summary:  "Drop completed jobs from the registry; running jobs are left alone.",
			Detail: "Always explicit: nothing reaps automatically when wait or kill " +
				"returns, because the script may still want to inspect $job after " +
				"the call. After 'reap', the 'jobs' listing reflects only entries " +
				"whose process is still running.",
		},
		{
			Name: "require", Handler: handleRequire,
			Category: categoryAssert,
			Usage:    "require <verb> [args...]  |  require <bool-expr>  |  require not <verb> [args...]",
			Summary:  "Check a condition; halt the script on failure.",
			Detail: "Verbs and operators are the same as assert. Use require where the " +
				"following statements only make sense if the condition holds.",
		},
		{
			Name: "trace", Handler: handleTrace,
			Category: categorySession,
			Usage:    "trace on  |  trace off",
			Summary:  "Toggle execution tracing on or off.",
			Detail: "With tracing on, each statement prints to stderr just before " +
				"it runs, prefixed with '+ file:line:' and rendered with " +
				"interpolations resolved -- argv with $name substituted by the " +
				"value (compact JSON for structured values), 'let' with the bound " +
				"value, 'defer' both at registration and at fire, foreach with the " +
				"loop variable per iteration. Useful for understanding what a " +
				"script actually saw at the moment a call was made. 'trace off' " +
				"disables it again. The CLI flag -x / --trace turns tracing on at " +
				"script startup.",
		},
		{
			Name: "source", Handler: handleSource, Complete: completeSourceArg,
			Category: categoryIO,
			Usage:    "source <file>",
			Summary:  "Execute commands from a file in the caller's session.",
			Detail: "Variables, defs, and jobs started in the file inherit the " +
				"caller's scope. Defers in the file fire when source returns " +
				"(file-as-cleanup-unit). Nested source is rejected. A relative " +
				"path resolves against the directory of the script containing the " +
				"`source` statement, matching Python's import / Ruby's " +
				"require_relative: a script can `source lib.bpfman` and find its " +
				"sibling regardless of where the user invoked the runner from. " +
				"Absolute paths and paths typed at the interactive prompt resolve " +
				"against the current working directory.",
		},
		{
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
		{
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
		{
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
		{
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
		{
			Name: "undef", Handler: handleUndef,
			Category: categorySession,
			Usage:    "undef <name>...",
			Summary:  "Remove user-defined commands.",
		},
		{
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
		{
			Name: "zip", Handler: handleZip,
			Category: categoryIO,
			Usage:    "zip <list> <list>",
			Summary:  "Pair two lists element-wise into a list of 2-element pair lists.",
			Detail: "zip is a pure builtin called from expression position: " +
				"'foreach (a b) in (zip $xs $ys) { ... }' or 'let pairs = zip $xs $ys'. " +
				"Length mismatch is a hard error rather than silent truncation: " +
				"parallel-list patterns carry an implicit \"these are paired\" " +
				"invariant, so dropping the tail of the longer list would convert " +
				"a bug into wrong-shape output. Empty + empty yields an empty list. " +
				"Multi-var foreach destructures each pair back into named bindings; " +
				"a single-var foreach binds the whole pair list and reaches the " +
				"elements via $pair[0] / $pair[1].",
		},
		{
			Name: "u32le", Handler: handleU32LE,
			Category: categoryIO,
			Usage:    "u32le <integer>",
			Summary:  "Encode an integer as a 4-byte little-endian hex string.",
			Detail: "Returns 8 lowercase hex characters with no 0x prefix. " +
				"Useful for `bpfman -g NAME=HEX` global-data injection " +
				"where the .bpf.c declares `volatile const __u32`. " +
				"Rejects negative inputs and values that exceed UINT32_MAX.",
		},
		{
			Name: "u64le", Handler: handleU64LE,
			Category: categoryIO,
			Usage:    "u64le <integer>",
			Summary:  "Encode an integer as an 8-byte little-endian hex string.",
			Detail: "Returns 16 lowercase hex characters with no 0x prefix. " +
				"Useful for `bpfman -g NAME=HEX` global-data injection " +
				"where the .bpf.c declares `volatile const __u64`. " +
				"Rejects negative inputs (Go uint64 max is the upper bound).",
		},
		{
			Name: "unset", Handler: handleUnset, Complete: completeUnsetArg,
			Category: categorySession,
			Usage:    "unset <name>...",
			Summary:  "Remove variable bindings.",
		},
		{
			Name: "vars", Handler: handleVars,
			Category: categorySession,
			Usage:    "vars",
			Summary:  "List session variables and their kinds.",
		},
		{
			Name: "version", Handler: handleVersion,
			Category: categoryMeta,
			Usage:    "version",
			Summary:  "Print version information.",
		},
		{
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
	} {
		repl.RegisterBuiltin(b)
	}

	// Register each effectful builtin's BindShape with the shell
	// package so the static checker (shell/check.go's inferBindShape)
	// resolves `<- NAME ARGS...` to the correct primary Shape
	// without a hand-maintained switch. Entries with a nil BindShape
	// (vars, help, jobs, reap, ...) bind nothing assignable
	// and so contribute no shape; inferBindShape's default takes
	// care of the rest (external-subprocess result).
	for name, b := range repl.Builtins() {
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
//
// A relative path argument resolves against the directory of the
// script that contains the `source` statement -- the same shape
// Python `import` and Ruby `require_relative` use, and what most
// readers expect. Absolute paths bypass this transform.
//
// Paths typed at the interactive prompt (where c.Pos.File is
// empty) anchor against the cwd captured at replLoop entry, not
// against the live process cwd at evaluation time. Indistinguishable
// in production (the shell has no cd builtin so cwd does not change
// during a loop), but it lets parallel tests inject a base dir via
// repl.WithInteractiveBaseDir instead of calling os.Chdir, which would
// race other t.Parallel tests reading the global cwd.
func handleSource(c builtinCtx) (shell.Value, error) {
	if c.Env == nil {
		return shell.Value{}, shell.SpanErrorf(c.Span, "source requires an active shell environment")
	}
	args := repl.ArgTexts(c.Args)
	if len(args) == 1 && !filepath.IsAbs(args[0]) {
		switch {
		case c.Pos.File != "":
			args[0] = filepath.Join(filepath.Dir(c.Pos.File), args[0])
		default:
			if base := repl.InteractiveBaseDir(c.Ctx); base != "" {
				args[0] = filepath.Join(base, args[0])
			}
		}
	}
	return shell.Value{}, repl.Source(c.Ctx, c.CLI, c.Mgr, c.Env, repl.SourceHooks{
		Fallback:       bpfmanFallback,
		BindFallback:   bpfmanBindFallback,
		MakeAssertStmt: makeExecAssertStmt,
	}, args)
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
	for n := range repl.Builtins() {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n+" ")
		}
	}
	for n := range repl.Keywords() {
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
