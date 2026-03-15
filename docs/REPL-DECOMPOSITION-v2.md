# REPL decomposition plan

## Current state

The `shell` package is in good shape. It owns language mechanics:
tokenisation, statement parsing, session state, expansion, and typed
argument output. The package boundary is correct.

The command layer already exists and is the right shape.
`command.go` contains a sealed `Command` interface, 17 concrete
typed command nodes (e.g. `ShowProgramCommand`, `LoadFileCommand`,
`LinkAttachCommand`), and a clean parse/exec split via
`parseCommand` and `execCommand`. The evaluation pipeline is
already `[]shell.Arg -> typed Command -> execCommand`. This is the
architecture the earlier plan called "Phase 1"; it is implemented.

The remaining architectural weakness is more specific: command
parsing still relies on `extractProgramID`, `extractProgramIDs`,
`extractLinkID`, and `extractLinkIDs`. These helpers collapse
domain meaning to a string, which is then reparsed into a typed ID.
The seam is inside command parsing, not at the higher-level
command-dispatch shape.

`command.go` is now the real god file (2400+ lines). `repl.go`
(1700 lines) is large but architecturally sound: a REPL loop, shell
builtins, assertions, completion, and thin argument glue. File
decomposition for both files should follow the architectural
refinement, not precede it.

## Why flat decomposition, not a subpackage

The existing `cmd/bpfman/` directory is already organised as a flat
command-oriented package: `attach.go`, `delete.go`, `detach.go`,
`dispatcher.go`, `doctor.go`, `gc.go`, `get.go`, `list.go`,
`load_file.go`, `load_image.go`, `unload.go`. The REPL
decomposition should follow the same pattern.

Introducing `cmd/bpfman/repl/` would create two different organising
schemes inside the same command: flat files for normal CLI commands,
subpackage for REPL. That asymmetry has to earn its keep, and right
now it does not.

Three specific reasons to stay flat:

1. **Consistency.** The directory already tolerates large files split
   by concern inside one package. REPL files can follow the same
   rule.

2. **Tight coupling.** The REPL code is heavily entangled with CLI,
   OutputFlags, command structs, formatters, manager.Manager, and
   completion informed by bpfman-specific nouns. A `repl/` package
   would not be a cleanly reusable package; it would mostly be a
   packaging convenience.

3. **Cost.** A file split is cheap and reversible. A package split
   means exported identifiers or indirection, more imports, more
   helper movement, and possible awkward cycles.

The problem is "god file", not "wrong package boundary". File
decomposition is the lower-risk move.

### When to reconsider a subpackage

Move to `cmd/bpfman/repl/` only if one of these becomes true:

- The REPL grows substantially beyond command coordination.
- The `cmd/bpfman/` package namespace gets too crowded.
- Tests become materially easier with a subpackage.
- A clear public surface for the REPL subsystem is needed.

### Why `shell/` stays top-level

`shell` is a general language layer: tokenisation, statement parsing,
variable/session model, value model, typed expansion. It is not
command-specific code. Moving it under `cmd/bpfman/shell/` would
imply it is merely a local implementation detail, which contradicts
the architecture.

## Guiding principle

The goal is to eliminate re-parsing, not to eliminate text.

Some things are naturally text: subviews like `maps`, formats like
`json`, selectors, file paths, assertion operands, flags and
subcommands. Plain text is fine for these.

The smell is not "string exists". The smell is "this is
semantically a program reference, but we turn it into text and then
rediscover that fact".

### Decision rule

Ask of each parser argument: does this argument have domain meaning
beyond plain text?

- Yes: parse it into a typed value immediately
  (`parseProgramIDArg`, `parseLinkIDArg`).
- No: plain text is fine (`maps`, `links`, `json`, `--recursive`).

## What is already implemented

- `Command` interface (sealed, with `isCommand()` marker).
- 17 concrete typed command nodes with paired `parse`/`exec`
  functions.
- `parseCommand(args []shell.Arg) (Command, error)` routing.
- `execCommand(ctx, cli, mgr, cmd) (shell.Value, error)` dispatch.
- Thin `replDispatch` orchestration.
- `parseProgramIDArg(a shell.Arg) (kernel.ProgramID, error)` —
  typed parser for program ID arguments, used by `show program`
  and `program get`.
- `parseLinkIDArg(a shell.Arg) (kernel.LinkID, error)` — typed
  parser for link ID arguments, used by `link get`.

The command architecture exists. The typed argument parsers exist.
The next step is to migrate the remaining commands to use them and
retire the old `extractProgramID`/`extractLinkID` helpers.

## Phasing

### Phase 1: typed domain parsers at the parser boundary

The remaining architectural work is to refine the typed command
layer so that domain references are parsed directly into typed
domain values at the parser boundary, instead of collapsing to
strings and being reparsed.

The typed domain parsers are:

```go
func parseProgramIDArg(arg shell.Arg) (kernel.ProgramID, error)
func parseLinkIDArg(arg shell.Arg) (kernel.LinkID, error)
```

These replace the old two-step path:

```go
// Before: shell.Arg -> string -> ParseXID -> wrapper -> .Value
idStr, err := extractProgramID(args[0])
parsed, err := ParseProgramID(idStr)
cmd := &ShowProgramCommand{ID: parsed.Value}

// After: shell.Arg -> kernel.ProgramID directly
id, err := parseProgramIDArg(args[0])
cmd := &ShowProgramCommand{ID: id}
```

The important point is that no command parser should go through
`shell.Arg -> string -> ParseXID`.

#### Migration status

Done:

1. **`show program`** — uses `parseProgramIDArg`.
2. **`program get`** — uses `parseProgramIDArg`.
3. **`link get`** — uses `parseLinkIDArg`.

Next:

4. **Plural-ID mutation commands**: `program unload`,
   `program delete`, `link detach`, `link delete`. These call
   `parseProgramIDArg` / `parseLinkIDArg` in a loop rather than
   going through `extractProgramIDs` / `extractLinkIDs`.

5. Remaining commands as warranted.

Once all consumers are migrated, remove `extractProgramID`,
`extractProgramIDs`, `extractLinkID`, and `extractLinkIDs`.

#### What not to attack first

Do not try to eliminate every `argTexts(...)` call. Some uses are
fine, especially for:

- Shell builtins.
- Assertion verbs that are inherently text-oriented.
- Flags and subcommands that are genuinely just words.

### Phase 2: split command.go by family

`command.go` is now the real god file. After the typed-ref
refinement, split it by command family:

```
cmd/bpfman/
  command.go                 # Command, parseCommand, execCommand
  command_program.go         # program/show/load parse+exec
  command_link.go            # link parse+exec
  command_dispatcher.go      # dispatcher list/get/delete parse+exec
  command_diag.go            # doctor/gc parse+exec
```

`parseCommand` and `execCommand` are pure routers. Command logic
does not accumulate in `command.go`; it lives in per-family files.

Each command has a paired parse and exec function:

```go
func parseShowProgram(args []shell.Arg) (*ShowProgramCommand, error)
func execShowProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *ShowProgramCommand) (shell.Value, error)
```

### Phase 3: split repl.go by concern

After command.go is decomposed, split repl.go by responsibility:

```
cmd/bpfman/
  repl.go                  # ReplCmd, Run
  repl_reader.go           # newReader, openScriptReader, replHistoryPath
  repl_loop.go             # replLoop, replEval, sourceLoc
  repl_shellcmd.go         # help, dump, vars, unset, source, version
  repl_assert.go           # assert/require subsystem
  repl_arg.go              # argText, argTexts, argToValue
  repl_complete.go         # top-level completion routing
  repl_complete_vars.go    # variable/path completion
  repl_complete_files.go   # path completion
```

Phase 3 is done when:

- `repl.go` contains only ReplCmd, Run, and perhaps very small
  top-level wiring.
- `repl_loop.go` reads as a compact orchestration pipeline.
- Shell builtins, assertions, completion, and reader bootstrap each
  live in their own files.
- Behaviour and tests are unchanged.

### Phase 4: reconsider package boundaries

Only after the dust settles, evaluate whether the REPL warrants its
own package. The flat file split may be sufficient indefinitely.

## What each file owns

### command.go

`Command` interface, `parseCommand`, `execCommand`, and any common
parse helpers shared by multiple command families.

`parseCommand` and `execCommand` are pure routers. Command logic
does not accumulate here; it lives in the per-family files.

### command_program.go (Phase 2)

Program-related command types and their parse/exec logic:
ShowProgramCommand, GetProgramCommand, UnloadProgramCommand,
DeleteProgramCommand, LoadFileCommand, LoadImageCommand,
ListProgramsCommand, and their parsers/executors.

### command_link.go (Phase 2)

Link-related command types and parse/exec logic:
LinkAttachCommand, LinkDetachCommand, LinkGetCommand,
LinkDeleteCommand, LinkListCommand, and their parsers/executors.

### command_dispatcher.go (Phase 2)

Dispatcher resource commands: DispatcherListCommand,
DispatcherGetCommand, DispatcherDeleteCommand, and their
parsers/executors.

### command_diag.go (Phase 2)

Diagnostics and maintenance commands: DoctorCommand, GCCommand,
and their parsers/executors.

### repl.go

ReplCmd, Run. Public entrypoints only.

This file answers: how do we start the REPL?

### repl_reader.go (Phase 3)

Input and bootstrap mechanics: newReader, openScriptReader,
replHistoryPath.

This file answers: where does input come from?

The `source` builtin in repl_shellcmd.go uses openScriptReader,
but the file-opening mechanics belong here, not with the builtins.

### repl_loop.go (Phase 3)

The REPL loop, source location tracking, and the evaluation
pipeline.

```go
type sourceLoc struct {
    file string
    line int
}

func replLoop(...)
func replEval(...)
```

This file answers: how does one line move through the pipeline?

Its control flow: tokenise, parse stmt, expand, shell builtin or
domain command, bind result if let.

`repl_loop.go` may know *when* to call shell builtins or domain
dispatch. It should not know *how* any particular command works. If
command-specific logic starts appearing here, that logic belongs in
per-command parsers or executors instead.

If the evaluation helpers (evalSet, evalLet, evalCommand) grow
enough to warrant separation, they can move to a separate
repl_eval.go. Do not split preemptively.

### repl_shellcmd.go (Phase 3)

Shell/session builtins: shellCommands, replShellCmd, replHelp,
replVars, replUnset, replDump, lookupBareVar, replSource,
replVersion, replContextKey, replSourcingKey.

These are not bpfman domain commands. They are REPL commands.

This file answers: what commands belong to the shell itself?

### repl_assert.go (Phase 3)

The assertion subsystem: errRequireFailed, assertResult,
replAssertRequire, evalAssertVerb, all assert helpers,
negateMessage, runCommand.

This is already a subsystem in its own right.

This file answers: how do assert/require behave?

### repl_arg.go (Phase 3)

Generic `shell.Arg` glue helpers: argText, argTexts, argToValue.

This file is transitional boundary glue, not a permanent centre of
gravity. It holds mechanical conversions at the shell/domain
boundary. It is allowed to stay small and boring. If
domain-specific logic starts appearing here, that logic belongs in
per-command parsers instead.

### repl_complete.go (Phase 3)

Top-level completion routing: replCommandNames, replSubcommands,
replAssertVerbs, replAttachTypes, showProgramViews, replCompleter,
replComplete, replCompleteArgs, replCompleteProgramDelete,
replCompleteProgramIDs, replCompleteLinkIDs,
replCompleteLinkAttach, replCompleteShowProgram.

This file answers: given a line and cursor position, what completion
family are we in?

Completion logic often expands quietly because it mirrors command
grammar. If completion starts duplicating parser logic, that is a
design smell, not just a file-size issue.

### repl_complete_vars.go (Phase 3)

Variable-aware completion: replCompleteVarPath,
replCompleteVarNames, varPathSuffix.

### repl_complete_files.go (Phase 3)

Filesystem completion: replFileCompletions.

## Target evaluation flow

```go
func replEval(...) error {
    tokens, err := shell.Tokenise(input)
    if err != nil {
        return reportErr(...)
    }

    stmt, err := shell.ParseStmt(tokens)
    if err != nil {
        return reportErr(...)
    }
    if stmt == nil {
        return nil
    }

    switch s := stmt.(type) {
    case *shell.SetStmt:
        return evalSet(session, s)

    case *shell.LetStmt:
        return evalLet(ctx, cli, mgr, session, s, loc)

    case *shell.CommandStmt:
        return evalCommand(ctx, cli, mgr, session, s, loc)

    default:
        return reportErr(...)
    }
}

func evalCommand(...) error {
    args, err := session.Expand(stmt.Tokens)
    if err != nil {
        return err
    }

    if handled, err := replShellCmd(..., args, ...); handled || err != nil {
        return err
    }

    cmd, err := parseCommand(args)
    if err != nil {
        return err
    }

    _, err = execCommand(ctx, cli, mgr, cmd)
    return err
}
```

Each phase does one kind of work. No phase re-discovers information
that an earlier phase already resolved.

## What not to do

- Do not split files before the architecture is cleaner. Nine smaller
  files with the same stringly weakness is not progress.
- Do not create `cmd/bpfman/repl/` up front. Solve "god file" with
  file splits, not package splits.
- Do not over-generalise command parsing into a framework before the
  actual command families need it.
- Do not move completion into `shell`. Completion is
  application-aware and belongs with the REPL client.
- Do not make `shell` know about assert, source, or bpfman resource
  nouns. `shell` should stay generic.
- Do not prematurely create errors.go or types.go files. Keep
  sentinels and small structs near their usage until they are
  genuinely shared broadly.
- Do not let `repl_arg.go` become a junk drawer. It is boundary
  glue, not a design centre.
- Do not try to eliminate every `argTexts(...)` call. The bad
  stringliness is not "text exists"; it is "typed domain meaning
  collapses to text too early".
- Do not over-correct into "everything must carry refs forever".
  Commands carry refs or IDs depending on what execution needs. The
  goal is to eliminate re-parsing at the parser boundary, not to
  force a uniform representation.
