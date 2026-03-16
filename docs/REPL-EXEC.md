# REPL exec command

## Summary

The `exec` shell command allows users to run small host-side helper
commands such as `ip`, `tc`, and `bpftool` during interactive REPL
sessions and scripts.

The purpose is not to embed a general shell into the REPL. The purpose
is to support the setup, inspection, and cleanup steps that naturally
surround `load`, `attach`, `doctor`, and related bpfman workflows.

The design is deliberately narrow:

- `exec` is a REPL shell command, not a bpfman domain command
- arguments are tokenised and expanded by the REPL, then passed
  directly to `exec.CommandContext`
- there is no `sh -c`
- there are no pipelines, redirects, globbing, or job control
- `exec` returns a structured result suitable for `let`
- non-zero exit status is treated as command failure

That gives the REPL a practical escape hatch without collapsing the
boundary between the REPL language and a full shell.

## Motivation

The REPL can inspect and manage bpfman state, but many realistic
workflows require preparing the host environment.

Typical examples include:

- creating or deleting dummy, veth, or netns-backed interfaces
- inspecting host state with `ip link`, `ip netns`, `tc`, or `bpftool`
- mounting or checking paths before load or attach operations
- writing simple REPL scripts that both prepare the system and then
  exercise bpfman commands
- asserting that a host-side helper command succeeds or fails during a
  test script

Without some form of host command execution, the user experience is
awkward:

- setup happens outside the REPL
- scripts cannot describe the full workflow
- `assert ok` and `assert fail` cannot cover the external steps that
  make the bpfman commands meaningful

A small `exec` command solves that problem.

## Goals

The feature satisfies the following goals.

### 1. Support setup and inspection helpers

Users can run commands such as:

- `exec ip link add dummy0 type dummy`
- `exec ip link set dummy0 up`
- `exec ip link show dummy0`
- `exec bpftool prog show`

### 2. Work in both interactive and script mode

The command behaves consistently in interactive REPL sessions and in
sourced or file-backed scripts.

### 3. Integrate with variables and assertions

Users can write:

- `let out = exec ip link show dummy0`
- `dump out.stdout`
- `assert contains $out.stdout dummy0`
- `assert ok exec ip link del dummy0`
- `assert fail exec ip link show does-not-exist`

### 4. Preserve the existing REPL model

The REPL remains a small command language with typed values and
explicit dispatch. `exec` fits into that model without introducing an
entirely different execution model.

## Non-goals

This feature is not intended to turn the REPL into a general-purpose
shell.

Specifically, it does not provide:

- a POSIX shell
- a command language with pipelines
- a redirection language
- a job-control environment
- a shell with glob expansion, command substitution, or subshells
- a replacement for writing a real shell script when that is the
  better tool

The following are explicitly out of scope:

- implicit shell invocation (the REPL never wraps commands in `sh -c`
  on the user's behalf; a user may still explicitly run `exec sh -c
  "..."` as the target program)
- `|`
- `>`, `>>`, `<`, `2>`
- `&&`, `||`, `;`
- background jobs
- streaming interactive subprocesses
- custom stdin piping from the REPL
- shell-style environment assignment syntax such as `FOO=bar cmd`
- per-command working-directory management
- REPL builtins that emulate large portions of bash

The implementation makes those non-goals obvious: `replExec` passes
the argv vector directly to `exec.CommandContext` and never invokes a
shell interpreter.

These non-goals describe functionality the REPL does not implement
itself. Users may still explicitly run external programs, including a
shell, and thereby opt into those programs' semantics.

## Why exec belongs in the shell layer

The REPL has two conceptual layers.

The first layer is the REPL shell language:

- `let`
- `set`
- `dump`
- `exec`
- `assert`
- `require`
- `source`
- `unset`
- `vars`
- `help`
- `version`

These commands manage the session, variables, and scripting
experience.

The second layer is the bpfman domain command layer:

- `program ...`
- `link ...`
- `dispatcher ...`
- `gc`
- `doctor`

These commands model bpfman operations.

`exec` belongs in the first layer, not the second.

That is because `exec` is not a bpfman resource operation. It is a
session-level utility for orchestrating the environment around bpfman
operations.

Treating it as a shell command preserves a clear architectural rule:

- shell commands manipulate the REPL session or its interaction with
  the host environment
- domain commands manipulate bpfman-managed state

That separation remains useful with `exec` present.

## User model

The user model is simple.

The REPL accepts a command line, tokenises it, expands variables, and
dispatches it either to:

- a shell command implementation, or
- a typed bpfman domain command parser and executor

`exec` follows that same pattern.

Examples:

    exec ip link add dummy0 type dummy
    exec ip link show dummy0
    let out = exec ip link show dummy0
    assert contains $out.stdout dummy0
    assert fail exec ip link del does-not-exist

The user does not have to learn shell quoting or shell expansion rules
beyond the REPL's own tokenisation rules.

## Syntax

The syntax is:

    exec <argv...>

Where `<argv...>` is one or more REPL tokens after normal REPL
tokenisation and variable expansion.

Examples:

- `exec ip link show lo`
- `exec tc qdisc show dev eth0`
- `let out = exec bpftool prog show`
- `assert ok exec ip link add dummy0 type dummy`

`exec` requires at least one argument after the command name. An empty
`exec` is an error.

## Semantics

### Tokenisation and expansion

The REPL performs its normal processing first:

1. tokenise input
2. parse statement form
3. expand variables
4. dispatch to the shell command implementation

That means:

- quoted strings remain single arguments
- `$var` expansion happens through the REPL's existing `shell.Session`
  model
- structured values are not passed directly to subprocesses; they are
  resolved to scalar text via `argTexts`

`exec` is argv-oriented. It does not serialise structured REPL values
for subprocess input. If a command needs a value from a structured
variable, the user must pass a scalar path such as
`$prog.record.program_id`.

### Process spawning

After expansion, `exec` constructs an argv vector via `argTexts` and
passes it directly to `exec.CommandContext`.

This is the key semantic constraint.

The REPL does not implicitly invoke:

- `sh -c`
- `bash -c`
- any other shell interpreter

Instead, it passes the argv vector directly to `exec.CommandContext`.
Users may still explicitly choose to run a shell, for example:

    exec sh -c "echo hello"

Shell syntax such as `|`, `>`, and `&&` has no special meaning to
`exec` itself. Those characters are just ordinary argv elements unless
the target program interprets them.

### Environment

The subprocess inherits the current process environment by default.

There is no custom environment model.

### Working directory

The subprocess runs in the current working directory of the bpfman
process.

There is no `cd` or per-command working-directory selection.

### Context and cancellation

The subprocess is created with the REPL command context so that
cancellation and timeout behaviour compose naturally with the rest of
the REPL.

If the REPL context is cancelled, the subprocess is terminated through
standard `CommandContext` behaviour.

## Result model

`exec` produces a structured result on success, represented by the
`execResult` type:

```go
type execResult struct {
    Argv     []string `json:"argv"`
    Stdout   string   `json:"stdout"`
    Stderr   string   `json:"stderr"`
    ExitCode int      `json:"exit_code"`
}
```

The fields are:

- `argv`: the exact argv vector executed
- `stdout`: captured standard output
- `stderr`: captured standard error
- `exit_code`: process exit status (always 0 on success)

The result is converted to a `shell.Value` using `ValueFromStruct`.

That allows idioms such as:

- `let out = exec ip link show dummy0`
- `dump out.stdout`
- `assert contains $out.stdout dummy0`
- `dump out.argv`

## Printing behaviour

The command makes a clear distinction between plain execution and
bound execution.

### Plain command form

When the user runs:

    exec ip link show dummy0

the command prints captured stdout to the REPL output stream on
success. Stderr is not printed on success; it is retained in the
structured result for `let` use if needed.

### Bound form

When the user runs:

    let out = exec ip link show dummy0

the command does not print normal command output. It returns the
structured value for binding.

This works because the `LetStmt` path in `replEval` calls
`replShellCmd` with `cli.WithDiscardOutput()`, so `replExec` writes
its stdout to a discarded writer. The structured result is unaffected.

## Failure model

Non-zero exit status is treated as command failure.

That means:

- `exec ...` returns an error when the subprocess exits non-zero
- `assert ok exec ...` fails on non-zero exit
- `assert fail exec ...` passes on non-zero exit

This fits the existing REPL model, where command failure is
represented as an error rather than as a value the caller must always
inspect.

The error message includes enough context to be useful:

- the full argv joined with spaces
- the exit status
- captured stderr when non-empty

For example:

    exec ip link del does-not-exist: exit status 1: Cannot find device ...

When the command is not found (not an exit error), the error wraps the
underlying OS error:

    exec nosuchcmd: exec: "nosuchcmd": executable file not found in $PATH

The implementation deliberately does not model shell-style
success/failure as a normal value path.

## Shell command dispatch change

Adding `exec` required a change to the shell command dispatch
contract. Previously `replShellCmd` returned:

- `handled` `bool`
- `err` `error`

That was enough for commands like `help` or `vars`, but not for shell
commands that produce an assignable result.

`exec` is the first shell-layer command that is assignable through
`let`.

The dispatch function now returns:

- `handled` `bool`
- `value` `shell.Value`
- `err` `error`

Existing shell commands return `shell.Value{}` (nil value) for the
value position. `exec` returns a non-nil structured value.

The `LetStmt` case in `replEval` was updated accordingly. Instead of
rejecting all shell commands in `let` context, it now:

1. calls `replShellCmd` with `cli.WithDiscardOutput()` to suppress
   printing
2. if the command was handled and produced a non-nil value, binds it
3. if the command was handled but produced no value, reports an error
   ("cannot bind result of ...")
4. if the command was not handled, falls through to domain dispatch

That allows shell commands that only print to remain non-assignable
while letting `exec` (and any future value-producing shell commands)
participate in `let` bindings.

## Command behaviour

The implemented behaviour is:

- `exec` is a shell command registered in `shellCommands`
- it requires at least one argv element
- it captures stdout and stderr into `bytes.Buffer` values
- on success:
  - plain form prints stdout to the REPL output stream
  - bound form returns a structured value without printing
- on non-zero exit:
  - it returns an error
  - stderr is incorporated into the error message when non-empty
- on command-not-found or other non-exit errors:
  - it returns an error wrapping the underlying OS error

## Examples

### Interactive setup

    exec ip link add dummy0 type dummy
    exec ip link set dummy0 up
    exec ip link show dummy0

### Capturing output

    let out = exec ip link show dummy0
    dump out.stdout
    assert contains $out.stdout dummy0

### Using with failure assertions

    assert ok exec ip link add dummy0 type dummy
    assert fail exec ip link add dummy0 type dummy
    assert ok exec ip link del dummy0

### Mixed bpfman workflow

    exec ip link add dummy0 type dummy
    exec ip link set dummy0 up
    let prog = load file -p ./xdp.o --programs xdp:pass
    let link = link attach xdp -i dummy0 $prog
    show program $prog links
    exec ip link del dummy0

### Variable expansion

    set iface = dummy0
    exec ip link add $iface type dummy
    let out = exec ip link show $iface
    assert contains $out.stdout $iface

### Stderr inspection

    let out = exec sh -c "echo errout >&2"
    assert contains $out.stderr errout

## Security and boundary considerations

`exec` runs arbitrary host commands as the same user as the bpfman
process. That is powerful, but it is not meaningfully more powerful
than the user already having shell access in the same environment.

This feature does not attempt to impose a partial security sandbox. A
weak sandbox would create false confidence.

The correct stance is simpler:

- `exec` is a convenience for trusted users in trusted environments
- it is intended for development, testing, debugging, and local
  operational workflows
- it remains explicit and minimal

The main safety mechanism is scope control, not faux isolation.

## Alternatives considered

### 1. Do nothing

Users can always leave the REPL and run shell commands separately.

This keeps the REPL smaller, but it weakens scripting and makes mixed
host-plus-bpfman workflows clumsy.

### 2. Implement helper-specific commands only

For example, add bespoke commands for interface creation or namespace
setup.

This sounds attractive at first, but it scales poorly. The set of
useful helpers is large and environment-specific. The REPL would start
accumulating thin wrappers over existing tools.

That is worse than a narrow `exec`.

### 3. Implicit sh -c wrapping

An alternative would have been for `exec` to always route commands
through `sh -c` internally. That would make the feature superficially
more powerful, but it would also:

- blur the REPL language boundary
- import shell quoting and shell expansion complexity
- make behaviour harder to reason about
- increase the risk of accidental feature creep into "mini-shell"

This is the wrong trade-off. The implementation avoids it by passing
the argv vector directly to `exec.CommandContext`. Users who need
shell features can still run `exec sh -c "..."` explicitly, making
the choice visible and intentional.

### 4. Make exec a domain command

This would put host-process execution into the same category as
`program`, `link`, and `dispatcher` management.

That is architecturally muddled. `exec` is not a bpfman domain
operation.

## Resolved questions

### 1. Should stderr be printed on successful plain exec?

No. Stdout is printed on success. Stderr is retained only in the
structured result and used in error construction on failure.

### 2. Should there be a text-only helper alongside exec?

Not implemented. If users often want "run command and return stdout
only", that can be revisited after the result model settles.

### 3. Should stdin ever be forwarded?

Not implemented. Foreground interactive subprocess handling is a much
bigger feature than simple helper execution.

### 4. Should the REPL eventually support a few more shell helpers?

Possibly. Any future helper should be judged against the same
boundary: useful session primitive, not shell-creep.

## Future considerations

The dispatch change that introduced `shell.Value` returns for shell
commands is general. If future shell commands need to produce
assignable results, they can follow the same pattern without further
architectural work.
