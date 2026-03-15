# REPL exec command

## Summary

Add an `exec` shell command to the bpfman REPL so that users can run
small host-side helper commands such as `ip`, `tc`, and `bpftool`
during interactive sessions and scripts.

The purpose is not to embed a general shell into the REPL. The purpose
is to support the setup, inspection, and cleanup steps that naturally
surround `load`, `attach`, `doctor`, and related bpfman workflows.

This document proposes a deliberately narrow design:

- `exec` is a REPL shell command, not a bpfman domain command
- arguments are tokenised and expanded by the REPL, then passed
  directly to `exec.CommandContext`
- there is no `sh -c`
- there are no pipelines, redirects, globbing, or job control
- `exec` may return a structured result suitable for `let`
- non-zero exit status is treated as command failure

That gives the REPL a practical escape hatch without collapsing the
boundary between the REPL language and a full shell.

## Motivation

Today the REPL can inspect and manage bpfman state, but many realistic
workflows still require leaving the REPL to prepare the host
environment.

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

The feature should satisfy the following goals.

### 1. Support setup and inspection helpers

Users should be able to run commands such as:

- `exec ip link add dummy0 type dummy`
- `exec ip link set dummy0 up`
- `exec ip link show dummy0`
- `exec bpftool prog show`

### 2. Work in both interactive and script mode

The command should behave consistently in interactive REPL sessions and
in sourced or file-backed scripts.

### 3. Integrate with variables and assertions

Users should be able to write:

- `let out = exec ip link show dummy0`
- `dump out.stdout`
- `assert contains $out.stdout dummy0`
- `assert ok exec ip link del dummy0`
- `assert fail exec ip link show does-not-exist`

### 4. Preserve the existing REPL model

The REPL should remain a small command language with typed values and
explicit dispatch. `exec` should fit into that model rather than
introducing an entirely different execution model.

## Non-goals

This feature is not intended to turn the REPL into a general-purpose
shell.

Specifically, it must not become:

- a POSIX shell
- a command language with pipelines
- a redirection language
- a job-control environment
- a shell with glob expansion, command substitution, or subshells
- a replacement for writing a real shell script when that is the
  better tool

The following are explicitly out of scope for the initial design:

- `sh -c`
- `|`
- `>`, `>>`, `<`, `2>`
- `&&`, `||`, `;`
- background jobs
- streaming interactive subprocesses
- custom stdin piping from the REPL
- shell-style environment assignment syntax such as `FOO=bar cmd`
- per-command working-directory management
- REPL builtins that emulate large portions of bash

The design should make those non-goals obvious in both implementation
and user-visible behaviour.

## Why exec belongs in the shell layer

The current REPL has two conceptual layers.

The first layer is the REPL shell language:

- `let`
- `set`
- `dump`
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

That separation remains useful even after `exec` is added.

## User model

The user model should be simple.

The REPL accepts a command line, tokenises it, expands variables, and
dispatches it either to:

- a shell command implementation, or
- a typed bpfman domain command parser and executor

`exec` should follow that same pattern.

Examples:

    exec ip link add dummy0 type dummy
    exec ip link show dummy0
    let out = exec ip link show dummy0
    assert contains $out.stdout dummy0
    assert fail exec ip link del does-not-exist

The user should not have to learn shell quoting or shell expansion
rules beyond the REPL's own tokenisation rules.

## Syntax

The proposed syntax is:

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
- structured values are not passed directly to subprocesses; they must
  first be resolved to scalar values where appropriate

### Process spawning

After expansion, `exec` constructs an argv vector and passes it
directly to `exec.CommandContext`.

This is the key semantic constraint.

The REPL does not invoke:

- `sh -c`
- `bash -c`
- any other shell interpreter

This means shell syntax is not recognised. Characters such as `|`,
`>`, and `&&` are just ordinary argv elements unless the target
program interprets them.

That is intentional.

### Environment

The subprocess inherits the current process environment by default.

The initial design does not add a custom environment model.

### Working directory

The subprocess runs in the current working directory of the bpfman
process.

The initial design does not add `cd` or per-command working-directory
selection.

### Context and cancellation

The subprocess should be created with the REPL command context so that
cancellation and timeout behaviour compose naturally with the rest of
the REPL.

If the REPL context is cancelled, the subprocess should be terminated
through standard `CommandContext` behaviour.

## Result model

`exec` should produce a structured result on success so it can be
bound with `let`.

A suitable result shape is:

- `argv` `[]string`
- `stdout` `string`
- `stderr` `string`
- `exit_code` `int`

For example, conceptually:

- `argv`: the exact argv vector executed
- `stdout`: captured standard output
- `stderr`: captured standard error
- `exit_code`: process exit status

This result should be convertible to `shell.Value` using the existing
`ValueFromStruct` mechanism.

That allows idioms such as:

- `let out = exec ip link show dummy0`
- `dump out.stdout`
- `assert contains $out.stdout dummy0`

## Printing behaviour

The command needs a clear distinction between plain execution and bound
execution.

### Plain command form

When the user runs:

    exec ip link show dummy0

the command should print captured stdout to the REPL output stream on
success.

This matches user expectation that `exec` behaves like a simple
command runner in interactive use.

For stderr on success, the simplest initial rule is:

- do not print stderr automatically on success
- retain it in the structured result for `let` use if needed later

That keeps ordinary output cleaner.

### Bound form

When the user runs:

    let out = exec ip link show dummy0

the command should not print normal command output. It should return
the structured value for binding.

This matches the existing REPL convention that `let` captures results
rather than printing them.

## Failure model

Non-zero exit status should be treated as command failure.

That means:

- `exec ...` returns an error when the subprocess exits non-zero
- `assert ok exec ...` fails on non-zero exit
- `assert fail exec ...` passes on non-zero exit

This is the best fit for the existing REPL model, where command
failure is represented as an error rather than as a value the caller
must always inspect.

The error message should include enough context to be useful, ideally:

- the command or argv
- the exit status
- captured stderr when non-empty

For example:

    exec ip link del does-not-exist: exit status 1: Cannot find device ...

The design deliberately does not model shell-style success/failure as
a normal value path.

## Shell command dispatch change

Adding `exec` exposes a useful gap in the current shell command layer.

At the moment, shell commands are handled by a function that returns
roughly:

- `handled` `bool`
- `err` `error`

That is enough for commands like `help` or `vars`, but it is not
enough for shell commands that produce an assignable result.

`exec` is the first clear example of a shell-layer command that should
be assignable through `let`.

So the shell command dispatch path should be adjusted to support an
optional `shell.Value` result.

Conceptually, the shell command contract should become something like:

- `handled` `bool`
- `value` `shell.Value`
- `err` `error`

or an equivalent small result type.

That allows:

- shell commands that only print and return no value
- shell commands that are assignable
- `let` bindings for shell commands where appropriate

This is the deeper architectural change. `exec` is simply the first
feature that requires it.

## Proposed command behaviour

The initial behaviour should be:

- `exec` is available as a shell command
- it requires at least one argv element
- it captures stdout and stderr
- on success:
  - plain form prints stdout
  - bound form returns a structured value
- on non-zero exit:
  - it returns an error
  - stderr is incorporated into the error message where useful

That gives a consistent and minimal model.

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

## Security and boundary considerations

`exec` runs arbitrary host commands as the same user as the bpfman
process. That is powerful, but it is not meaningfully more powerful
than the user already having shell access in the same environment.

This feature should not attempt to impose a partial security sandbox.
A weak sandbox would create false confidence.

The correct stance is simpler:

- `exec` is a convenience for trusted users in trusted environments
- it is intended for development, testing, debugging, and local
  operational workflows
- it should remain explicit and minimal

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

### 3. Use sh -c

This would make the feature superficially more powerful, but it would
also:

- blur the REPL language boundary
- import shell quoting and shell expansion complexity
- make behaviour harder to reason about
- increase the risk of accidental feature creep into "mini-shell"

This is the wrong trade-off.

### 4. Make exec a domain command

This would put host-process execution into the same category as
`program`, `link`, and `dispatcher` management.

That is architecturally muddled. `exec` is not a bpfman domain
operation.

## Open questions

There are a few points worth deciding explicitly before
implementation.

### 1. Should stderr be printed on successful plain exec?

A conservative initial answer is no.

Print stdout on success, but keep stderr only in the structured result
or use it in error construction on failure.

### 2. Should there be a text-only helper alongside exec?

Possibly later, but not initially.

If users often want "run command and return stdout only", that can be
revisited after the result model settles.

### 3. Should stdin ever be forwarded?

Not in the first version.

Foreground interactive subprocess handling is a much bigger feature
than simple helper execution.

### 4. Should the REPL eventually support a few more shell helpers?

Possibly. But `exec` should land first, and any future helper should
be judged against the same boundary: useful session primitive, not
shell-creep.

## Recommendation

Proceed with a narrow `exec` design.

Implement it as a shell-layer command with direct argv execution,
captured output, structured success results, and ordinary error-based
failure semantics.

Before coding, make one small architectural change:

- allow shell-layer commands to return an optional `shell.Value`

That keeps the design clean and avoids special-casing `exec` later.

The important thing is not merely adding command execution. The
important thing is adding it without surrendering the REPL's current
clarity of purpose.
