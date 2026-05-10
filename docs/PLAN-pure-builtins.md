# Plan: Pure Builtins as Expression-Callables

## Overview

This document plans the work to formalise the boundary between
expression-space and command-space in the bpfman shell DSL, by
tagging builtins as pure or effectful in the registry, removing the
ad-hoc special cases that grew up around the first pure builtins
(`u32le`, `u64le`), and (later, gated on real friction) extending
the parser so pure builtins are invocable in expression position.

The architectural rationale lives in the design memo
(`project_repl_design_philosophy.md`, section "Architectural
invariants"). This document is the implementation plan: what
changes, in what order, with what acceptance criteria.

## Motivation

Two builtins were added recently -- `u32le` and `u64le` -- to
encode integers as little-endian hex strings for `bpfman -g
NAME=HEX` global-data injection. They are pure functions: integer
in, string out, no side effects, no rc envelope.

The shell currently dispatches them through the same `<-` form as
effectful builtins (`start`, `wait`, `exec`):

```
let pid_le <- u32le 12345
```

This works but carries two costs:

1. **Static checker friction.** `shell/check.go`'s `inferBindShape`
   has to special-case the names `u32le` and `u64le` to infer
   `OriginScalar` instead of the default `OriginEnvelope`. Every
   future pure builtin (`range`, `len`, `hex`, `format`, ...) would
   land another special case.

2. **Notational dishonesty.** The `<-` form means "run a command
   and bind its primary result". For a pure value computation this
   reads as ceremony. The grammar treats two semantically distinct
   things the same way.

The fix is to make purity an explicit registry property, then let
the static checker and (eventually) the parser consult that
property rather than carrying named exceptions.

## Phases

### Phase 1 -- Registry tagging (no grammar change)

Add an explicit purity tag to the builtin registry. The static
checker reads the tag instead of name-matching.

**Acceptance:** every existing script passes; the named special
case for `u32le` / `u64le` in `shell/check.go` is gone; the
registry is the single source of truth for purity.

#### File-level changes

1. **`cmd/bpfman-shell/builtins.go`**: add two fields to the
   `builtin` struct -- an explicit Effect enum and the call's
   declared arity.

   ```go
   type Effect int

   const (
       // EffectCommand is the default. The builtin participates
       // in the captured-result protocol: its handler runs
       // through the bind path, the Rc envelope carries ok /
       // code / stdout / stderr, and `<-` is the only way to
       // invoke it. Examples: start, wait, kill, exec, source.
       EffectCommand Effect = iota

       // EffectPure marks a deterministic value-producing
       // builtin: no subprocess, no side effects, no rc
       // envelope. The handler returns a typed Value that
       // flows directly to the caller, whether the call site
       // uses `<-` (compatibility spelling) or `=` (Phase 2,
       // expression-position invocation). Pure builtins can
       // still fail; failure is an evaluation error, not a
       // command result.
       EffectPure
   )

   type builtin struct {
       Name     string
       Handler  func(builtinCtx) (shell.Value, error)
       Complete argCompleter

       Effect Effect
       Arity  int // exact number of positional arguments;
                  // ignored for EffectCommand (variadic by
                  // convention).

       Category string
       Usage    string
       Summary  string
       Detail   string
   }
   ```

   The `Effect` enum (rather than a bare `Pure bool`) makes the
   design vocabulary explicit: every builtin has a declared
   effect, and the language's two-worlds rule maps directly to
   the two enum values. Adding a third effect kind in future
   (e.g. `EffectQuery` for read-only-but-IO-bearing primitives)
   stays a registry concern, not a grammar concern.

   `Arity` is the number of positional arguments a pure builtin
   takes. The parser and checker consume exactly that many
   argument expressions in expression position; see Phase 2.
   For effectful builtins the field is ignored (their argv is
   variadic in the shell sense).

2. **`cmd/bpfman-shell/builtins.go`** (registry entries): mark the
   pure builtins with `Effect: EffectPure` and a declared
   `Arity`. As of this writing:

   - `jq`: pure, arity 2 (filter, value)
   - `u32le`: pure, arity 1 (integer)
   - `u64le`: pure, arity 1 (integer)
   - everything else: effectful (default `Effect: EffectCommand`,
     no change to existing entries)

3. **`shell/check.go`**: replace the named special case in
   `inferBindShape` with a registry lookup. The shell package
   does not currently import the `cmd/bpfman-shell` registry; the
   simplest plumbing is to register the names of pure builtins
   with the checker at process init, alongside the existing
   `RegisterShape` calls in `cmd/bpfman-shell/kindshapes.go`. A
   small `RegisterPureBuiltin(name string, returnShape Shape)`
   API on the checker side, called once per pure entry from the
   shell main, eliminates the import-cycle question and keeps
   the checker out of the cmd-side registry.

4. **`shell/check.go`** (doc comment on `inferBindShape`): the
   three-family doc gets simplified. With purity in the
   registry, there is no "third family"; there are command-
   shaped binds (rc + envelope or typed payload) and pure-bind
   short-circuits (the pure builtin's typed Value flows through
   directly).

#### Tests

- `cmd/bpfman-shell/le_helper_test.go` already covers the
  encoding semantics of `u32le` / `u64le`.
- Add a check-level test that confirms a `let X <- u32le 1234`
  bind produces a Scalar shape via the registry, with no
  hard-coded name list in `inferBindShape`.

#### Out of scope

- Grammar changes. `<-` remains the only way to invoke any
  builtin. The new tag is consulted by the bind path, not by
  the parser.

### Phase 2 -- Expression-position invocation (grammar change)

Pure builtins become callable in expression position with
shell-style syntax.

**Acceptance:** `let x = u32le 1234` parses and runs; effectful
builtins remain command-only (`let x = wait $job` is a parse-time
or check-time error); existing `<-` invocations continue to work
for backward compatibility.

#### File-level changes

1. **`shell/parse.go`**: when parsing an expression-position
   identifier, consult the pure-builtin registry. If the name
   is registered with `Effect: EffectPure`, parse a call
   expression: identifier followed by exactly `Arity` primary
   expressions (literals, var refs, parenthesised
   subexpressions). The registry-declared arity is the rule;
   the parser does not look at trailing tokens to decide where
   the call ends. If the name is not a registered pure builtin,
   fall back to the current var-ref parse.

2. **`shell/expr.go`**: new AST node `PureCallExpr{Name, Args}`.
   Eval dispatches to the same handler the bind path uses; the
   returned Value flows directly into the surrounding expression
   evaluator.

3. **`shell/check.go`**: extend `inferExprShape` to recognise
   `PureCallExpr` and look up the registered return shape.

4. **Effectful-leak refusal**: the parser's pure-builtin
   recognition consults the `Pure: true` tag. An effectful
   builtin (`wait`, `exec`, ...) in expression position is **not**
   recognised as a call; it falls through to var-ref, which fails
   because no variable of that name is defined. The error message
   is the "undefined variable" the checker already produces. The
   command-space leak the design memo forbids is therefore
   structurally impossible: the parser cannot dispatch an
   effectful builtin from expression position because the
   registry tag gates the recognition.

#### Migration

- `e2e/new/*.bpfman` scripts may migrate `let X <- u32le N`
  bindings to `let X = u32le N`. The migration is mechanical and
  optional; the `<-` form continues to work.
- The migration is the moment to delete the explicit `let X = ...`
  forms that currently extract `${X.stdout}` from the bound
  envelope, since the pure-builtin call returns the underlying
  value directly.

#### Out of scope

- `def` does not gain expression-position invocation. The two-
  worlds rule keeps user-defined commands command-shaped.
- No parens-form (`u32le(1234)`). Shell-style only.
- No nested calls (`hex(u32le(x))`). If the corpus ever forces
  this, that is Phase 3 territory.

#### Acceptance criteria

- `let x = u32le 1234` parses, runs, binds `x` to a Scalar.
- `let x <- u32le 1234` continues to work (backward compatible).
- `let x = wait $job` produces an "undefined variable" error
  (or a clearer "command-only" error if we add it).
- Existing scripts pass without modification.
- Migrated scripts read more cleanly.

### Phase 3 -- Parens / nested calls

**Probably never.** The aesthetic constraint (expressions look
shell-ish, not ALGOL-ish) rules this out by default.

Revisit only if the corpus forces nested invocation often enough
that the two-line workaround becomes a recurring papercut, not
just stylistic friction. Until then, the discipline is:

```
let h = hex $x
let y = u32le $h
```

over

```
let y = u32le(hex(x))
```

If revisited, the design move is well-defined: add parens around
arg lists, accept comma-separated args, decide on operator-
precedence interactions. None of that work belongs to Phase 1 or
Phase 2.

## Considerations

### Why `<-` continues to work for pure builtins

Even after Phase 2 ships, `let X <- u32le 1234` remains valid as
a **compatibility spelling** for the same pure value bind that
`let X = u32le 1234` produces. The two forms are equivalent for
pure builtins: both bind `X` to the scalar result; neither
produces an rc envelope.

This is deliberate. If a builtin is pure, it is not promised to
have an rc envelope as part of its conceptual model. It can
still fail -- a parse error, a range violation -- but that
failure is an **evaluation error** (the script halts, same as
any other expression error), not a command result the script can
inspect via `$r.ok`. Promising a captured-result envelope for
pure builtins would let purity leak back into command-space and
defeat the whole point of the registry split.

The Phase 2 affordance is therefore additive on the spelling but
identical on the semantics: the `=` form becomes available for
pure builtins, the `<-` form still does what it always did, and
both bind the same Value. The registry tag (`Effect: EffectPure`)
is what tells both the parser and the bind path to short-circuit
the envelope wrapping.

### Effectful builtins must be unrepresentable in expression position

The whole point of the architectural invariant is that
expression-space cannot perform commands. The Phase 2 parser
change must enforce this structurally, not by convention. The
implementation rule: **the parser's expression-position lookup
consults the `Pure: true` tag specifically.** An effectful
builtin in expression position is not a "wrong but parseable"
construct -- it is unrepresentable. The error the user sees is
the same error they would see for an undefined identifier.

If a future builtin author tries to mark `wait` or `exec` as
pure, that is a registration-time review concern, not a grammar
concern. The grammar refuses the leak by construction.

### `def` stays command-oriented

The temptation, once pure builtins become expression-callable, is
to extend `def` so user-defined commands can become
expression-callable too:

```
def hex(x) { ... return ... }
let y = hex 42
```

That is the leak. Resist it. `def` bodies can `start` jobs, can
`assert`, can `defer`, can call external commands -- they are
inherently command-shaped, and giving them expression-position
invocation lets command-space leak into expression-space through
the back door.

The two-worlds rule: pure builtins are a registry-tagged bridge
maintained by the language implementor. User code stays in
command-space. If a script needs computational power, jq is the
escape hatch.

### Risks / open questions

1. **Argument parsing in expression position.** Where does an
   argument list end? Resolved by **registry-declared arity**.
   The parser, on recognising a pure-builtin name in expression
   position, looks up `Arity` in the registry and consumes
   exactly that many primary expressions. Subsequent tokens
   are not part of the call.

   ```
   let x = u32le 1234 + 5    -- (u32le 1234) + 5; arity 1
   let y = format "%d" 42    -- format("%d", 42); arity 2
   let z = jq ".foo" $blob   -- jq(".foo", $blob); arity 2
   ```

   This is robust against future arity changes (a new pure
   builtin with arity 3 reads as `f a b c + d` =
   `(f a b c) + d`) and against nesting questions: nesting
   needs explicit grouping (`u32le (jq ".x" $v)`), which is
   parens for grouping the *expression*, not parens for the
   *call*. The grammar rule (Phase 3 forbidden territory) says
   parens are for grouping; calls remain space-separated.

2. **Nullary pure builtins.** `let now = clock` -- is that a
   variable reference or a zero-arg call? With the arity rule,
   a registered builtin with `Arity: 0` is unambiguously a
   call: the parser recognises the name, looks up arity 0, and
   consumes zero arguments. The shadowing question (what if a
   user variable is named `clock`?) stays the same as for any
   pure builtin: the registry wins, and users learn not to
   shadow registered names.

3. **Static-check warnings vs errors.** A pure builtin called
   with the wrong arity is a check-time error, not a runtime
   error. The check pass already knows the registry, and the
   arity field gives it the predicate directly: `len(args) !=
   builtin.Arity`. The error names the builtin and the expected
   arity; the user fixes the call site.

## Future extensions

- A `format` pure builtin (`let s = format "%d" 42`) would
  remove some of the residual `exec sh -c 'printf ...'` shell-
  outs that survive in tests for shell features other than LE
  encoding.
- A `range` pure builtin (`foreach i in (range 3) { ... }`) is
  already on the language polish queue and lands as another
  pure-builtin user.
- A `parseInt`/`parseFloat` pair would let scripts do safer
  numeric coercion than `|> jq tonumber`.

Each is small, each is registry-tagged at registration, each
flows through the same machinery this plan establishes.

## References

- Design memo: `project_repl_design_philosophy.md`, section
  "Architectural invariants" (the two-worlds rule, shell-ish
  not ALGOL-ish, jq as escape hatch).
- Current pure-builtin instances: `cmd/bpfman-shell/le_helper.go`,
  `cmd/bpfman-shell/le_helper_test.go`.
- Current registry: `cmd/bpfman-shell/builtins.go`.
- Current static-checker special case: `shell/check.go`,
  `inferBindShape` `case "u32le", "u64le":`.
- Polish queue context: `project_repl_design_philosophy.md`,
  section "Polish queue", item 9 (`range N` builtin).
