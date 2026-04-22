# REPL implementation notes

This document records internal design choices, deferred work, and
architectural context that would not fit cleanly into
`language.md` or `builtins.md`. It is for future maintainers.

## Package boundary

- **`shell/`** owns the language: tokenisation, statement parsing,
  expression AST, evaluation, session state, typed values, and
  origin kinds. Pure package, no I/O.
- **`cmd/bpfman/`** owns domain semantics: bpfman command parsing,
  execution against `manager.Manager`, rendering, and the REPL
  dispatch loop. Depends on `shell/`.

`shell/` knows nothing about bpfman commands. Domain code consumes
`shell.Expr`, `shell.Value`, `shell.Arg`, and friends through their
public API.

## Evaluation pipeline

One accumulated input (possibly spanning lines in continuation
mode) flows through:

```
input string
  → shell.Tokenise      ([]Token)
  → shell.ParseProgram  ([]Stmt)
  → for each Stmt:
       → Expand          ([]Arg)          -- variable and cmdsub expansion
       → ParseExpr        (Expr)           -- for let/if condition
       → Eval             (Value)          -- with CmdRunner for [cmd]
     or:
       → replShellCmd / replDispatch      -- for CommandStmt
  → session.Set on let
```

Each stage has a single responsibility. The `CmdRunner` closure is
what lets expressions execute domain commands at evaluation time;
without it, CmdSub is a runtime error.

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

`shell.ParseExpr` recognises three shapes:

- Single arg → `LiteralExpr` or `VarRefExpr` or `CmdSubExpr`
  (primary).
- Two args → unary predicate.
- Three args → binary comparison (middle must be an operator).
- More than three args → error with a `wrap commands in [ ]` hint.

There is no precedence beyond "comparisons are the outermost
operator"; parenthesisation is not supported because nothing in the
current grammar needs it. If `!`/`&&`/`||` ever arrive, that will
change.

## if/elif/else block parsing

Block boundaries are single-character `{`/`}` word tokens. The
block parser balances nested braces (for nested `if`) and calls
`ParseProgram` recursively on the inner token sequence. Statement
separators inside blocks are newline or `;`.

Conditions extract tokens between the keyword (`if`/`elif`) and the
opening `{`, strip any `TokenSep` entries, and hand the result to
`ParseExpr`. This lets multi-line conditions (unusual but legal)
work.

## Known deferred work

- **No truthiness.** Intentional (see `language.md` non-goals). If
  this is ever revisited, `shell.AsBool` is the place to relax the
  rule.
- **`nil` is a prefix verb, not a unary predicate.** The current
  `Expand` pipeline refuses to carry nil values through as
  arguments ("variable X is null"), so a bare-name operand is the
  only way to inspect nil-ness. Moving `nil` into the Expr grammar
  would require changing `Expand` to emit a dedicated nil arg.
- **Completion for expressions.** Current completion is
  keyword-and-noun driven; it doesn't deeply understand the
  expression grammar (operator position, cmdsub scope). Acceptable
  for now.
- **File god-objects.** `cmd/bpfman/command.go` (~2400 lines) and
  `cmd/bpfman/repl.go` (~2000 lines) remain large. A flat
  file-level decomposition (split by command family) is cheaper
  than a subpackage; deferred until the complexity forces it.
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

Design rationale for each step is no longer tracked separately
once the change landed; `git log` is authoritative.
