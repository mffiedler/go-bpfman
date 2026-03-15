# REPL language: design warts and incremental fix plan

This document captures known design issues in the `replang` package
and the REPL consumer code in `cmd/bpfman/repl.go`, together with a
phased plan to resolve them.

The target is a clean package boundary:

- `replang` owns the language: lexing, statement parsing, session
  state, expansion, and typed argument output.
- `cmd/bpfman/repl.go` owns bpfman command semantics and I/O: domain
  command parsing, execution against `manager.Manager`, and rendering.

The purpose of these changes is not abstraction for its own sake. It
is to make each phase explicit, reduce stringly-typed re-parsing, and
keep the REPL easy to extend as new commands are added.

## Current state

The implementation is solid. The package is small, pure, well-tested,
and the separation between tokenisation, line parsing, session
expansion, and runtime values is good. The test coverage exercises
behaviour, not just happy paths.

What follows is not a list of bugs. It is a list of places where the
design can be tightened to support growth without accumulating
workarounds.

## Design principles

The refactor follows a small set of language-design principles:

- **Preserve structure as long as possible.** Do not collapse
  meaningful syntax into plain strings before later phases have had a
  chance to consume it in typed form.
- **Make phases explicit.** Lexing, statement parsing, expansion,
  command parsing, execution, and rendering should each do one kind of
  work.
- **Parse into explicit node types.** Distinct statement and command
  variants should be represented directly, not encoded implicitly in
  booleans, optional fields, or string conventions.
- **Keep the language no larger than necessary.** The REPL remains
  line-oriented and command-oriented. It does not need general
  expressions, operator precedence, or block structure.

---

## Warts

### Wart 1: structured variable references lose identity during expansion

This is the most important issue.

`Session.Expand` converts bare structured variable references into
plain word tokens:

```go
if v.IsStructured() {
    result = append(result, Token{Kind: TokenWord, Text: tok.Text})
    continue
}
```

This has three consequences:

1. Command parsers see variable syntax (`$prog`) after expansion,
   violating the intended phase contract that expansion should resolve
   or deliberately preserve references in a typed form before command
   parsing.

2. Downstream functions (`resolveProgramIDArg`, `resolveVarRefs` in
   `repl.go`) re-parse the `$` prefix out of plain strings and call
   `session.Get` again, creating a second variable-resolution
   mechanism.

3. The `Origin()` type-safety check (commit 8971522) works correctly
   but is reached through an unnecessarily indirect path: the token
   kind is discarded, the text is converted to a string, the string
   is re-lexed to find the `$` prefix, the variable name is
   re-extracted, and only then is the value looked up again.

The information flow today:

```
TokenVarRef -> Expand -> TokenWord{Text:"$prog"} -> tokenTexts
  -> []string -> strings.HasPrefix("$") -> session.Get -> Origin()
```

Three representations of the same fact.

### Wart 2: `Line` is an implicit sum type encoded as a product

`Line` uses a string field (`VarName`), a boolean discriminator
(`IsSet`), and the absence of `VarName` to distinguish three
variants:

```go
type Line struct {
    VarName string
    Command []Token
    IsSet   bool
}
```

The codebase's own modelling conventions (CLAUDE.md) say: "approximate
sum types with package-local sealed interfaces plus concrete structs."
`Line` violates this. The three variants should be explicit:

```go
type Stmt interface { isStmt() }

type LetStmt struct {
    Name    string
    Command []Token
}

type SetStmt struct {
    Name  string
    Value Token
}

type CommandStmt struct {
    Tokens []Token
}
```

This makes the variants self-documenting and eliminates the implicit
encoding. Consumers pattern-match on the interface rather than
inspecting fields and booleans.

### Wart 3: `doc.go` is stale

`doc.go` still describes the older shape of the language:

- Examples use `prog = load file ...`, not `let prog = ...`.
- It presents `$` expansion mostly as scalar substitution.
- The structured pass-through behaviour is documented as an aside
  rather than as the core language rule it now is.

The package documentation should reflect:

- `let` for command-result bindings
- `set` for scalar bindings
- The real semantics of bare structured variable passing: after
  expansion, scalar references are resolved eagerly, but structured
  references survive as typed arguments for command-level handling
- The distinction between `let` and `set` as part of the language core

### Wart 4: tokeniser/parser invariant leak around `=`

The tokeniser will not emit `TokenAssign` at the start of a line
because `isTokenStart` requires at least one preceding token:

```go
func isTokenStart(tokens []Token) bool {
    return len(tokens) > 0
}
```

Input `= foo` produces a word token `=`, not `TokenAssign`.

The parse tests include a "leading equals is a parse error" case with
a leading `TokenAssign`, which the tokeniser cannot produce. This is a
parser/tokeniser invariant leak: the test exercises a state that
cannot arise from the tokeniser.

Keep the parser's rejection logic (it is defensive and cheap). Either
annotate the test as exercising a defensive path, or remove it and
rely on the tokeniser's guarantee.

### Wart 5: variable path syntax is more permissive than intended

`lexBareVarRef` accepts trailing dots (`$prog.`) by including the dot
in the path and stopping. `lexBracedVarRef` accepts anything up to
`}` as a path. `parsePath` is similarly forgiving about malformed
segments.

Examples that are accepted but probably should not be:

- `$prog.` (trailing dot)
- `${prog..id}` (empty segment)
- `${prog[abc]}` (non-numeric index)
- `${prog.maps[0].}` (trailing dot after index)

The intended grammar for variable reference paths is:

```
varref     := '$' ident path? | '${' ident path? '}'
path       := segment+
segment    := '.' ident | '[' digits ']'
ident      := [a-zA-Z_][a-zA-Z0-9_]*
digits     := [0-9]+
```

Variable references should be syntactically validated as early as
possible, with lexing rejecting clearly malformed reference forms.
Braced and bare forms should accept exactly the same path grammar,
keeping them interchangeable. Bare `$prog.foo.bar` is the normal
form. Braces are supported but not promoted; they become essential
only if the language later grows string interpolation or adjacency
concatenation. Until then, variable references are whole-token
syntax.

### Wart 6: information loss at the dispatch boundary

The conversion from `[]replang.Token` to `[]string` via `tokenTexts`
is the current information-loss boundary. After this point, token
kind, variable-reference structure, and any future typed argument
information are unrecoverable without re-parsing strings.

`replEval` calls `tokenTexts(expanded)` to convert the expanded token
slice to `[]string`, then passes that to `replDispatch`. All command
handlers work with raw strings. Every command handler that accepts
variable references must then re-discover them by inspecting string
prefixes.

### Wart 7: missing typed command IR

The code parses each line into a `Line`, then dispatches on string
patterns to handler functions that internally re-parse their arguments
(often via Kong). There is no intermediate typed command
representation between the parser and execution.

The typed command nodes do not need to be a grand hierarchy. For this
language, they are small and domain-shaped:

```go
type Command interface { isCommand() }

type LoadFileCmd struct { ... }
type ShowProgramCmd struct { ... }
type LinkAttachCmd struct { ... }
// etc.
```

Each command node carries resolved arguments (scalars as strings,
structured references as `Value`), eliminating the need for
`resolveProgramIDArg` and `resolveVarRefs` entirely.

### Wart 8: duplicated resolution logic by domain type

Argument resolution is currently duplicated by domain type:

- `resolveProgramIDArg` / `resolveProgramIDArgs`
- `resolveLinkIDArg` / `resolveLinkIDArgs`
- `resolveVarRefs` (generic fallback)

Each of these re-parses the `$` prefix, splits on `.` or `[`,
calls `session.Get`, optionally walks a path, optionally checks
`Origin()`, and optionally tries an implicit field path (e.g.
`.record.program_id` for programs, `.record.id` for links).

This is more than duplication. It is a sign that command-specific
typed argument parsing belongs after expansion, in the typed command
layer. Once command nodes carry resolved arguments, these helpers
disappear.

### Wart 9: `replDispatch` mixes language and domain concerns

`replDispatch` currently handles both shell-language commands and
bpfman domain commands in a single switch:

Language and session commands:

- `assert`, `require`
- `dump`, `vars`, `unset`
- `source`
- `help`, `version`

Domain commands:

- `program` (load, get, delete, unload, list)
- `link` (attach, detach, get, list, delete)
- `show program`
- `dispatcher` (list, get, delete)
- `doctor`, `gc`

As the language grows, this conflation makes the switch harder to
reason about. Language commands are statement-level concerns that
`replEval` should handle directly. Domain commands flow through the
`Command` layer.

---

## Target architecture

The target pipeline is:

```
read line
  -> replang.Tokenise
  -> replang.ParseStmt
  -> replang.ExpandStmt
  -> replang produces typed, structured arguments
  -> cmd/bpfman parses those into typed bpfman command nodes
  -> execute
  -> render
  -> update session
```

`replang` is a general shell language layer. `cmd/bpfman/repl.go` is
a client and executor over that layer.

### What `replang` owns

**Tokens.** Already in the right place: `Token`, `TokenKind`,
`Tokenise`, variable-reference lexing, comment stripping, identifier
validation.

**Statement AST.** Replace `Line` with an explicit sum type:

```go
type Stmt interface { isStmt() }

type LetStmt struct {
    Name string
    Cmd  []Token
}

type SetStmt struct {
    Name  string
    Value Token
}

type CommandStmt struct {
    Cmd []Token
}
```

**Session and Value.** Already in the right place: `Session`,
variable store, `Value`, `Lookup`, `LookupValue`. `Origin()` is
optional domain metadata carried on the value, not part of the core
shell semantics.

**Typed expansion output.** This is the key missing piece. Expansion
should not return `[]Token` that are later flattened to strings. It
should return a typed argument representation that preserves the
distinction between scalar-expanded values and structured references:

```go
type Arg interface { isArg() }

type WordArg struct {
    Text string
}

type QuotedArg struct {
    Text string
}

type ScalarValueArg struct {
    Text string
}

type StructuredValueArg struct {
    Name  string
    Value Value
}
```

`Arg` is the post-expansion representation of a command argument. It
preserves distinctions that would be lost in `[]string`: whether an
argument was literal command syntax, a quoted literal, an eagerly
resolved scalar value, or a structured shell value passed directly
to a command.

- `WordArg` is literal command text supplied by the user: command
  names, flags, paths, numeric IDs. It was never a variable
  reference.
- `QuotedArg` preserves user quoting as a distinct syntactic form
  when command parsers care about that distinction (e.g. a quoted
  path with spaces is distinct from an unquoted flag). The boundary
  should not erase it.
- `ScalarValueArg` is a value produced by variable expansion. The
  original variable reference has been resolved to a string. It is
  semantically distinct from `WordArg` because it came from a
  variable, not from user-typed literal text.
- `StructuredValueArg` is a resolved structured variable value
  (`Value`) passed directly to a command. The command parser decides
  how to use it (e.g. extract `.record.program_id`).

Expansion produces `[]Arg`, not `[]Token` and not `[]string`. This
is the contract between `replang` and its clients. Clients never need
to re-parse `$` prefixes or re-discover variable references from
strings.

**Shell-language commands.** `replang` should understand binding,
unbinding, and variable inspection. At minimum: `let`, `set`, `unset`,
`vars`, `dump`. These are language-level concerns, not domain
concerns. `source`, `assert`, and `require` may stay partly
in the REPL client layer, but the language-level mechanics belong in
`replang`.

### What `cmd/bpfman/repl.go` owns

**Domain command AST.** Typed bpfman command nodes that are specific
to this application:

```go
type Command interface { isCommand() }

type ShowProgramCmd struct {
    ProgramRef ProgramRefArg
    View       string
    Select     []string
    Format     string
}

type LoadFileCmd struct { ... }
type LinkAttachCmd struct { ... }
// etc.
```

These are not general language constructs. They encode bpfman command
semantics.

**Domain argument types.** Typed argument wrappers that absorb the
current resolution helpers:

```go
type ProgramRefArg struct {
    ID    *kernel.ProgramID
    Value *replang.Value
}

type LinkRefArg struct {
    ID    *kernel.LinkID
    Value *replang.Value
}
```

This is where the logic from `resolveProgramIDArg`,
`resolveLinkIDArg`, and `resolveVarRefs` moves. Those functions
disappear into typed argument parsing.

**Domain command parsing.** Each command family parses from expanded
structured args into typed command nodes:

- `parseShowProgram(args []replang.Arg) (ShowProgramCmd, error)`
- `parseLoadFile(args []replang.Arg) (LoadFileCmd, error)`
- `parseLinkAttach(args []replang.Arg) (LinkAttachCmd, error)`

**Execution and rendering.** Typed commands are executed against
`manager.Manager`. Rendering, formatting, tab completion, and CLI
output remain in the REPL client layer.

### The end state

```go
stmt, err := replang.ParseStmt(tokens)
expanded, err := replang.ExpandStmt(session, stmt)

switch s := expanded.(type) {
case *replang.LetStmt:
    cmd, err := replcmd.Parse(s.Cmd)
    result, err := executor.Execute(cmd)
    session.Set(s.Name, result.AssignValue)

case *replang.SetStmt:
    session.Set(s.Name, s.Value)

case *replang.CommandStmt:
    cmd, err := replcmd.Parse(s.Cmd)
    result, err := executor.Execute(cmd)
    renderer.Render(result)
}
```

Each phase does one kind of work. No phase re-discovers information
that an earlier phase already resolved.

### The final package boundary

`replang` exports:

- `Token`, `Tokenise`
- `Stmt` (`LetStmt`, `SetStmt`, `CommandStmt`), `ParseStmt`
- `Arg` (`WordArg`, `QuotedArg`, `ScalarValueArg`,
  `StructuredValueArg`), `ExpandStmt`
- `Session`, `Value`

`cmd/bpfman/` uses:

- Typed bpfman command nodes (`Command` interface)
- Command parsers from `[]replang.Arg`
- Executors against `manager.Manager`
- Renderers and completers

---

## Incremental fix plan

The fixes are ordered so that each step is self-contained, testable,
and valuable on its own. No step depends on a later step.

### Step 1: introduce `Stmt` sum type [DONE]

Replace the `Line` struct with a sealed `Stmt` interface and concrete
types (`LetStmt`, `SetStmt`, `CommandStmt`). `ParseLine` becomes
`ParseStmt`. Update all consumers.

This is mechanical and improves the boundary immediately without
breaking expansion semantics.

**Scope**: `replang/parse.go`, `replang/parse_test.go`,
`cmd/bpfman/repl.go`

### Step 2: preserve structured refs through expansion [DONE]

Change `Session.Expand` so that structured references are preserved
in typed form instead of being downgraded to `TokenWord`. Update the
session tests.

After this step, expansion resolves scalar references eagerly but
leaves structured references intact in typed form for command
parsing.

This is a `replang`-only change. It breaks the consumer in `repl.go`,
which step 3 fixes.

**Scope**: `replang/session.go`, `replang/session_test.go`

### Step 3: introduce typed `Arg` representation in `replang` [DONE]

Introduce the `Arg` interface (`WordArg`, `QuotedArg`,
`ScalarValueArg`, `StructuredValueArg`). Expansion produces `[]Arg`
instead of `[]Token`.

This makes the expansion contract explicit: clients receive typed,
structured arguments and never need to inspect token kinds or string
prefixes.

**Scope**: `replang/` (new file or in `session.go`)

### Step 4: remove `tokenTexts` from the main path [DONE]

Make `replEval` pass structured expanded arguments (`[]Arg`) forward
instead of collapsing to `[]string`. Where command handlers currently
call `resolveProgramIDArg` or `resolveVarRefs` to re-parse `$`
prefixes, pass the typed `Arg`, or an explicitly resolved `Value`,
directly.

This is the moment `cmd/bpfman/repl.go` becomes a client rather than
a re-parser. The `Origin()` type check remains, but the value arrives
through the type system rather than through string re-parsing.

**Scope**: `cmd/bpfman/repl.go`, `cmd/bpfman/repl_test.go`

### Step 5: update `doc.go` [DONE]

Rewrite the package documentation to reflect:

- `let` and `set` as the binding keywords
- The `Stmt` variants
- The expansion contract: scalar refs resolve to `ScalarValueArg`,
  structured refs become `StructuredValueArg`
- The `Arg` types as the public expansion output
- The distinction between `let` (command result) and `set` (scalar
  literal)

This moves after step 4 because the documentation should describe
the real API, not a transitional state.

**Scope**: `replang/doc.go`

### Step 6: tighten tokeniser invariants [DONE]

- Annotate or remove the parse test for leading `TokenAssign` (the
  tokeniser cannot produce it).
- Reject trailing dots in `lexBareVarRef` at lex time.
- Validate path segments inside `lexBracedVarRef` using the same
  grammar as bare refs.
- Add test cases for malformed variable references.

**Scope**: `replang/token.go`, `replang/token_test.go`,
`replang/parse_test.go`

### Step 7: split shell-language commands from domain commands [DONE]

In `replEval`, after `ParseStmt` and expansion, handle
shell-language commands and control commands (`unset`, `vars`, `dump`,
`source`, `assert`, `require`, `help`, `version`) outside the domain
command layer. Only domain command statements flow to the REPL client
layer for typed command parsing and execution.

This gives a clean separation between shell/session semantics and
bpfman semantics.

**Scope**: `cmd/bpfman/repl.go`

### Step 8: introduce typed bpfman command nodes [IN PROGRESS]

Define a `Command` interface in `cmd/bpfman/` with concrete types per
command family. Add a command-parsing phase between expansion and
execution. Each command node carries resolved arguments.

This is the largest step and should be done incrementally, one command
family at a time. A reasonable order:

1. `show program` (high-frequency, exercises structured refs) [DONE]
2. `load file` / `load image` (exercises result binding)
3. `link attach` / `link detach` (exercises cross-type refs)
4. `program get` / `link get` (exercises value return)
5. Remaining commands

Each command family can be migrated independently. The `replDispatch`
switch shrinks as typed nodes absorb its cases.

**Scope**: new file(s) in `cmd/bpfman/`, plus incremental changes to
`cmd/bpfman/repl.go`

### Step 9: typed command nodes absorb `replDispatch`

Once all command families have typed nodes, the large `switch` block
in `replDispatch` is replaced by a single type-switch on `Command`.
The `resolveProgramIDArg`, `resolveProgramIDArgs`, `resolveLinkIDArg`,
`resolveLinkIDArgs`, and `resolveVarRefs` functions are deleted.

**Scope**: `cmd/bpfman/repl.go`

### Step 10: update `REPL-LANG.md` design document

Update the design document to reflect the final architecture: the
`replang` / `cmd/bpfman` boundary, the `Stmt` and `Arg` types, the
expansion contract, and the typed command pipeline.

**Scope**: `docs/REPL-LANG.md`

---

## Definition of done

### `replang` is complete when

- It fully owns lexing, statement parsing, session state, and
  expansion.
- It does not know anything about bpfman commands.
- It never requires downstream clients to re-parse `$` prefixes out
  of strings.
- It hands clients typed, structured arguments (`[]Arg`) rather than
  lossy text.

### `cmd/bpfman/repl.go` is a client when

- It does not tokenise or parse language syntax itself.
- It does not re-discover variable references from strings.
- It only parses bpfman command semantics from typed arguments.
- It executes typed command nodes against `manager.Manager`.
- It renders outputs.

---

## What this does not change

- The `Value` type and `Origin()` mechanism remain as-is. They solve
  a different problem (domain-level type safety) and are orthogonal
  to the phase separation fixes.
- The session variable store remains a simple `map[string]Value`.
- The language remains line-oriented and command-oriented. There are
  no expression trees, operator precedence, or multi-line blocks.
- The renderer and projection model are unaffected.
