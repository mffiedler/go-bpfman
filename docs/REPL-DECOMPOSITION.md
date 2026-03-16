# REPL decomposition plan

## Current state

The `shell` package is in good shape. It owns language mechanics:
tokenisation, statement parsing, session state, expansion, and typed
argument output. The package boundary is correct.

`cmd/bpfman/repl.go` is architecturally sound but still a god object.
It wears too many hats: REPL loop, file handling, shell command
handling, assertion framework, completion, command parse/dispatch,
rendering helpers, and domain argument extraction glue.

The goal is to make the evaluation pipeline, shell builtins,
assertions, completion, and domain dispatch separately readable,
without changing behaviour and without introducing a new package
boundary.

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

## Next steps

### Immediate: typed domain refs at the parser boundary

The highest-value architectural move is replacing string-returning
extraction helpers with typed domain refs. This is the remaining
stringly seam: `shell` gives `[]shell.Arg`, but domain meaning
("this argument is a program reference") still collapses to a plain
string too early.

Build these first, before any file split beyond Phase 1:

```go
type ProgramRef struct {
    ID kernel.ProgramID
}

func parseProgramRef(arg shell.Arg) (ProgramRef, error)
func parseProgramRefList(args []shell.Arg) ([]ProgramRef, error)

type LinkRef struct {
    ID kernel.LinkID
}

func parseLinkRef(arg shell.Arg) (LinkRef, error)
func parseLinkRefList(args []shell.Arg) ([]LinkRef, error)
```

#### Migration order

1. **`show program`** (first target). Cleanest demonstration:
   exercises bare structured refs (`$prog`), scalar refs (`$pid`),
   explicit path refs (`$prog.record.program_id`), origin checking,
   one typed domain argument, minimal mutation logic.

   Target shape:

   ```go
   type ShowProgramCommand struct {
       Program ProgramRef
       View    string
       Output  OutputFlags
   }
   ```

2. **`program get` / `link get`**. Same pattern, same payoff.

3. **Plural-ID mutation commands**: `program unload`,
   `program delete`, `link detach`, `link delete`. These benefit
   from `parseProgramRefList` / `parseLinkRefList`.

4. Remaining commands as warranted.

#### What not to attack first

Do not try to eliminate every `argTexts(...)` call. Some uses are
fine, especially for:

- Shell builtins.
- Assertion verbs that are inherently text-oriented.
- Flags and subcommands that are genuinely just words.

The bad stringliness is not "text exists". It is "typed domain
meaning collapses to text too early".

#### Decision rule

Ask of each parser argument: does this argument have domain meaning
beyond plain text?

- Yes: parse it into a typed value immediately (`ProgramRef`,
  `LinkRef`).
- No: plain text is fine (`maps`, `links`, `json`, `--recursive`).

## Phasing

### Phase 1: split the god file

Move code into files without changing APIs or behaviour.

Target shape after Phase 1:

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

Phase 1 is done when:

- `repl.go` contains only ReplCmd, Run, and perhaps very small
  top-level wiring.
- `repl_loop.go` reads as a compact orchestration pipeline.
- Shell builtins, assertions, completion, and reader bootstrap each
  live in their own files.
- Behaviour and tests are unchanged.

Phase 1 is the default stopping point. Live with it for a while
before deciding whether further splitting is needed. The remaining
architectural pressure will be much easier to see clearly after the
god file is gone.

### Phase 1.5: typed domain refs

Introduce `ProgramRef`, `LinkRef`, and their parsers. Migrate
`show program` first, then `program get` / `link get`, then
plural-ID commands. This can happen independently of or alongside
Phase 1; it is architectural work, not file layout.

### Phase 2: split command families, only if still needed

After Phases 1 and 1.5, evaluate whether the REPL domain command
layer still feels heavy. Only then consider splitting:

```
cmd/bpfman/
  repl_command.go            # Command, parseCommand, execCommand
  repl_command_program.go    # program/show/load parse+exec
  repl_command_link.go       # link parse+exec
  repl_command_dispatcher.go # dispatcher list/get/delete parse+exec
  repl_command_diag.go       # doctor/gc parse+exec
```

Prefer introducing typed refs for any parser substantially rewritten
during this phase. Do not churn stable code just to remove a tiny
helper unless that helper is in the way.

### Phase 3: absorb remaining extraction glue

Replace any surviving extractProgramID/extractLinkID with typed
domain argument parsing. Command nodes hold typed references instead
of partially resolved strings.

Do this where it pays for itself, not as a blanket rewrite.

### Phase 4: reconsider package boundaries

Only after the dust settles, evaluate whether the REPL warrants its
own package. The flat file split may be sufficient indefinitely.

## What each file owns

### repl.go

ReplCmd, Run. Public entrypoints only.

This file answers: how do we start the REPL?

### repl_reader.go

Input and bootstrap mechanics: newReader, openScriptReader,
replHistoryPath.

This file answers: where does input come from?

The `source` builtin in repl_shellcmd.go uses openScriptReader,
but the file-opening mechanics belong here, not with the builtins.

### repl_loop.go

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

### repl_shellcmd.go

Shell/session builtins: shellCommands, replShellCmd, replHelp,
replVars, replUnset, replDump, lookupBareVar, replSource,
replVersion, replContextKey, replSourcingKey.

These are not bpfman domain commands. They are REPL commands.

This file answers: what commands belong to the shell itself?

### repl_assert.go

The assertion subsystem: errRequireFailed, assertResult,
replAssertRequire, evalAssertVerb, all assert helpers,
negateMessage, runCommand.

This is already a subsystem in its own right.

This file answers: how do assert/require behave?

### repl_arg.go

Generic `shell.Arg` glue helpers: argText, argTexts, argToValue.

This file is transitional boundary glue, not a permanent centre of
gravity. It holds mechanical conversions at the shell/domain
boundary. It is allowed to stay small and boring. If
domain-specific logic starts appearing here, that logic belongs in
per-command parsers instead.

### repl_complete.go

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

### repl_complete_vars.go

Variable-aware completion: replCompleteVarPath,
replCompleteVarNames, varPathSuffix.

### repl_complete_files.go

Filesystem completion: replFileCompletions.

### repl_command.go (Phase 2)

Domain command IR and two-stage dispatch: the Command interface,
parseCommand, execCommand, and any common parse helpers shared by
multiple command families.

parseCommand and execCommand are pure routers. Command logic does
not accumulate here; it lives in the per-family files.

### repl_command_program.go (Phase 2)

Program-related command types and their parse/exec logic:
ShowProgramCommand, ProgramGetCommand, ProgramUnloadCommand,
ProgramDeleteCommand, LoadFileCommand, LoadImageCommand,
ListProgramsCommand, and their parsers/executors.

Each command has a paired parse and exec function:

```go
func parseShowProgram(args []shell.Arg) (*ShowProgramCommand, error)
func execShowProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *ShowProgramCommand) (shell.Value, error)
```

### repl_command_link.go (Phase 2)

Link-related command types and parse/exec logic:
LinkAttachCommand, LinkDetachCommand, LinkGetCommand,
LinkDeleteCommand, LinkListCommand, and their parsers/executors.

### repl_command_dispatcher.go (Phase 2)

Dispatcher resource commands: DispatcherListCommand,
DispatcherGetCommand, DispatcherDeleteCommand, and their
parsers/executors.

Dispatcher is a resource family like program and link, not a
diagnostic tool, so it gets its own file.

### repl_command_diag.go (Phase 2)

Diagnostics and maintenance commands: DoctorCommand, GCCommand,
and their parsers/executors.

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

- Do not create `cmd/bpfman/repl/` up front. Solve "god file" with
  file splits, not package splits.
- Do not create all Phase 2 files immediately. Phase 1 may be
  sufficient.
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
