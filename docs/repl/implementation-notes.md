# REPL implementation notes

This document records internal design choices, deferred work, and
architectural context that would not fit cleanly into
`language.md` or `builtins.md`. It is for future maintainers.

## Package boundary

- **`shell/`** owns the language: tokenisation, parsing to a typed
  AST, expression and statement evaluation, session state, typed
  values, and origin kinds. Pure package, no I/O.
- **`cmd/bpfman/`** owns domain semantics: bpfman command parsing,
  execution against `manager.Manager`, rendering, and the REPL
  dispatch loop. Depends on `shell/`.

`shell/` knows nothing about bpfman commands. Domain code consumes
`shell.Program`, `shell.Stmt`, `shell.Expr`, `shell.Value`,
`shell.Arg`, and friends through their public API.

## Evaluation pipeline

One accumulated input (possibly spanning lines in continuation
mode) flows through:

```
input string
  → shell.Tokenise      ([]Token)
  → shell.Parse         (*Program)     -- full AST, with Loc on every node
  → shell.EvalProgram   (walks AST against *Env)
       → *LetStmt       : EvalExpr(RHS) → Value; session.Set
       → *IfStmt        : EvalExpr(Cond) → Bool; run matching branch
       → *CommandStmt   : EvalArgs(Args) → []Arg; env.ExecCommand
       → *CmdSubExpr    : EvalArgs on inner command; env.ExecSubstitution
```

`Env` bundles the `*Session` with two dispatch callbacks that the
REPL plugs in:

- `ExecCommand(args []Arg) (Value, error)` — top-level commands.
  Output is visible on the CLI; the returned Value is ignored for
  plain command statements.
- `ExecSubstitution(args []Arg) (Value, error)` — commands inside
  `[ ... ]`. Output is suppressed; a nil result is an error.

Command substitutions are parsed **eagerly**: the inner text is
tokenised and parsed into a nested `*Program` at `Parse` time, so
syntax errors inside the brackets surface before any statement
runs.

The evaluator produces `[]Arg` at the dispatch boundary via
`EvalArgs`, which maps each `Expr` to the appropriate arg variant:
literals to `WordArg`/`QuotedArg`, variable references to
`ScalarValueArg`/`StructuredValueArg` based on shape, adapter
expressions to `AdapterArg`, and nested command substitutions to
`ScalarValueArg`/`StructuredValueArg` based on the dispatched
value's shape. Command parsers type-switch on `Arg` exactly as
before.

For assertion evaluation, where the `assert` command receives an
already-evaluated `[]Arg` and needs to re-interpret it as a
comparison or predicate, `shell.ExprFromArgs` rebuilds an `Expr`
tree which is then evaluated through `EvalExpr` against a
runner-less `Env`.

## Source locations

Every `Token` carries a `Loc{Line, Col}` (1-based, byte columns)
recorded against the original input by the tokeniser.  The
`stripComment` helper replaces comment bodies with spaces so
offsets remain faithful across stripped comments. Every AST node
embeds a `Loc` picked up from its first token. Parse errors cite
the location via the private `locErrorf` helper and render as
`line:col: message`.

Intra-`[ ... ]` tokens are lexed in isolation against the inner
text, so their `Loc` values are relative to the substitution's
start, not to the outer source. Errors inside a command
substitution therefore point at the right structure but not at the
right absolute position. That is a known limitation left over from
the AST refactor; offsetting inner tokens against the outer `[` is
the next thing to revisit.

## Continuation mode

The REPL reads one line at a time but accumulates input across
lines when a brace or bracket is left open. `contState` tracks
brace and bracket depth, ignoring quoted content and per-line
comments. When depth returns to zero, the accumulated buffer is
handed to `replEval`.

EOF while a block is still open surfaces as `unterminated block at
end of input`. Unterminated strings are caught at tokenisation time
when the accumulated input is parsed.

## Origin types

Origin kinds are formalised as a closed set on `shell.Value`. Every
value-producer in the code declares its origin; consumers (typed
command parsers, `AsBool`) check it via `ExpectOrigin` before
structural extraction. `OriginUnknown` is the wildcard default for
untagged values (JSON round-trip, map literals), which preserves
the fallback behaviour of path lookup when a producer didn't tag.

To add a new domain type (say, `OriginDispatcher`):

1. Add the constant to `shell/origin.go`.
2. Tag the relevant producer (e.g. `GetDispatcherCommand` in
   `cmd/bpfman/command.go`).
3. Teach the consuming command parsers to accept it via
   `ExpectOrigin(..., OriginDispatcher)`.

## Expression grammar boundaries

`parseExpression` (used for `let` RHS and `if`/`elif` conditions)
recognises three shapes:

- Single token → primary (`LiteralExpr`, `VarRefExpr`,
  `AdapterExpr`, or `CmdSubExpr`).
- Two tokens → unary predicate followed by a primary.
- Three tokens → primary, binary operator, primary.
- Any other count → parse error with a "wrap commands in `[ ]`"
  hint implicit in the behaviour of `let x = cmd arg`, which now
  fails at parse time.

There is no precedence beyond "comparisons are the outermost
operator"; parenthesisation is not supported because nothing in the
current grammar needs it. If `!` / `&&` / `||` ever arrive, that
will change.

Adapter expressions (`file:$var`) are only legal as command
arguments, not as expression operands. Using one as a let RHS or if
condition is a runtime error emitted by `EvalExpr`.

## if/elif/else block parsing

Block boundaries are single-character `{`/`}` word tokens. The
block parser balances nested braces (for nested `if`) and drives
`parseStmts` recursively against the inner token stream with a
stop predicate on `}`. Statement separators inside blocks are
newline or `;`.

Conditions are collected as the token run between the keyword
(`if`/`elif`) and the opening `{`, with intervening `TokenSep`
entries stripped, then handed to `parseExpression`. Multi-line
conditions (unusual but legal) work naturally.

## Parse-only check mode

`bpfman-shell --check` (or `-c`) reads the same input as a regular
REPL invocation but stops after tokenise and parse: no Session,
Manager, or evaluator is constructed. It reports each chunk's
first error with a `file:line:` prefix followed by the tokeniser
or parser's own `line:col:` marker, and exits non-zero if any
chunk errored.

Two environmental carve-outs make check mode usable without root:
`rootExempt` in `main.go` grants the no-root path to `--check`,
and `NewCLI` skips logger initialisation when the repl subcommand's
`Check` field is set. The shipped `/etc/bpfman/bpfman.toml` is
typically only readable by root, and check mode needs nothing from
it.

## Known deferred work

- **No truthiness.** Intentional (see `language.md` non-goals). If
  this is ever revisited, `shell.AsBool` is the place to relax the
  rule.
- **`nil` is a prefix verb, not a unary predicate.** The current
  `EvalArgs` pipeline refuses to carry nil values through as
  arguments ("variable X is null"), so a bare-name operand is the
  only way to inspect nil-ness. Moving `nil` into the Expr grammar
  would require a dedicated Expr variant that preserves the null.
- **Intra-cmdsub error positions are chunk-relative.** See the
  source-locations section above.
- **Completion for expressions.** Current completion is
  keyword-and-noun driven; it doesn't deeply understand the
  expression grammar (operator position, cmdsub scope). Acceptable
  for now.
- **File god-objects.** `cmd/bpfman/command.go` and
  `cmd/bpfman/repl.go` remain large. A flat file-level
  decomposition (split by command family) is cheaper than a
  subpackage; deferred until the complexity forces it.
- **Typed inspection API consolidation.** The existing
  `inspect.ProgramView`, `coherency.Finding`, and
  `Manager.GCPlan`/`Manager.Get*` types cover all consumer needs.
  A separate `ProgramDetail`/`DispatcherDetail`/`CoherencyReport`
  layer proposed in early drafts has not landed and is not blocking
  anything.

## Testing strategy

- `shell/*_test.go` — unit tests for tokeniser, parser, expression
  evaluator, origin types, and assertions. Pure logic, no I/O.
- `cmd/bpfman/repl_test.go` — end-to-end REPL tests that feed
  input strings through `replLoop` and assert on session state or
  stderr. These catch dispatch and integration issues that pure
  unit tests can miss.
- `cmd/bpfman/repl_check_test.go` — parse-only coverage for
  `--check` including the emacs syntax-gallery smoke test.

Run:

```
direnv exec . make test
```

`sudo make test-e2e` exercises the full manager/kernel stack and
is not required for language-only changes.

## History

The current language is the result of a phased refactor:

1. Origin kinds formalised (`OriginKind`, `ExpectOrigin`).
2. Expression grammar introduced (`Expr`, `ParseExpr`, `Eval`);
   `assert` and `let` migrated to consume it.
3. Command substitution `[cmd]` added; `set` keyword retired in
   favour of `let` + expressions.
4. `if`/`elif`/`else` added with multi-line block continuation and
   strict boolean condition checking.
5. AST refactor: the old shallow parser (statement bodies kept as
   `[]Token`, expressions re-parsed per evaluation) was replaced
   with a full tokens → `*Program` → tree-walking evaluator
   pipeline. `Session.Expand` and the `CmdRunner` plumbing were
   retired in favour of `EvalArgs` and an `*Env` with
   `ExecCommand`/`ExecSubstitution` callbacks. A parse-only
   `--check` mode was added alongside.

Design rationale for each step is no longer tracked separately
once the change landed; `git log` is authoritative.
