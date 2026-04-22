# REPL builtins

Shell-language commands extend the REPL beyond the bpfman domain
without collapsing the boundary between the REPL and a general
shell. This document covers the host-integration builtins: `exec`,
`json`, and `file` adapters. For the shell-language surface itself
(`let`, `if`, `assert`, `source`, ...), see `language.md`.

## Philosophy

The REPL is a debugger, not a workflow engine. Builtins exist where
real workflows demand host integration — creating test interfaces,
parsing JSON output from `bpftool`, diffing map dumps — but the set
is deliberately narrow. Each builtin is documented here; adding a
new one is a language decision, not a local convenience.

## exec

`exec CMD [ARGS...]` runs a host command and returns an
`exec.result` value. The command's stdout is printed by default;
`let r = [exec ...]` captures without printing.

### Return value

The result structure:

```
{
  argv:      ["cmd", "arg1", ...],
  stdout:    "...",
  stderr:    "...",
  exit_code: 0
}
```

Accessible via path: `$r.stdout`, `$r.exit_code`, `$r.argv[0]`.

### Exit status semantics

`exec` distinguishes between two styles:

- `exec CMD ...` — non-zero exit is an **error**, surfacing the
  stderr to the REPL caller. Intended for setup/teardown steps that
  must succeed.
- `exec status CMD ...` — non-zero exit is **data**, captured in
  `exit_code`. The caller decides what to do. Intended for tools
  whose exit code is informative (e.g. `diff`).

```
exec ip link add dummy0 type dummy       # must succeed
let r = [exec status diff file:$a file:$b]
if $r.exit_code != 0 { dump r.stdout }
```

### Variable expansion and file adapters

`exec` argument tokens go through the usual expansion pipeline.
File adapters (see below) are recognised in argument position:

```
let data = [json parse '{"a":1,"b":2}']
let r    = [exec jq '.a' file:$data]
```

### Quoting when shelling to bash or sh

The REPL's own string syntax is deliberately simple: inside a
`"..."` or `'...'` literal every character is kept verbatim, with
no backslash escapes. `$` is therefore literal inside any DSL
quoted string, and only starts a variable reference when it
appears *unquoted* in argument position. In practice this means
shelling to `bash` composes cleanly as long as the DSL quote style
differs from the inner shell's:

```
# DSL single-quoted; bash sees the literal script unchanged and
# interprets its own $$, $1, $(...) etc.
let r = [exec bash -c 'echo "pid=$$"; jq ". | length" "$1"' sh file:$data]

# DSL double-quoted is fine too, provided the inner script does
# not use double quotes. The DSL passes the contents through to
# bash verbatim, so bash's own $$ still expands.
let r = [exec bash -c "echo hi; echo pid=$$"]
```

Do **not** try to backslash-escape inner quotes of the same type:
`"\"inner\""` ends the outer string at the first `\"` (because
`\` is a literal character, not an escape), so everything after
it is tokenised outside a string — a following `$` with no
identifier then trips the tokeniser. Pick a different outer quote
style instead.

If a variable has to reach the shell, hand it to `bash -c` as a
positional argument and read it inside the script as `$1`, `$2`,
and so on. That keeps DSL expansion and shell expansion separate:

```
let mid = $prog.status.maps[0].id
let sum = [exec bash -c 'bpftool map dump id "$1" -j | jq -j "[.[0].formatted.values[].value] | add"' sh $mid]
```

### What exec does not do

- No pipelines, redirects, globbing, or job control.
- No `sh -c` unless you explicitly invoke `sh`.
- No implicit variable expansion of `$`-prefixed strings inside
  quotes; that is the REPL's concern, not the host shell's.

## json parse

`json parse TEXT` decodes a JSON string into a structured value
tagged `OriginJSONParsed`. Combined with `exec`, it handles tools
that produce JSON output (`bpftool prog show --json`, `ip -j link
show`, `tc -j`).

```
let raw  = [exec bpftool prog show --json]
let data = [json parse $raw.stdout]
dump data[0].name
```

Numbers in the decoded tree are preserved as `json.Number` to avoid
float precision loss during round-trips.

### Errors

- Malformed JSON → parse error.
- Trailing data after a valid value → error (JSON must be a single
  top-level value).
- Empty input → decode error.

### Output formatting

Used as a top-level command (not in a `let` RHS), `json parse`
prints the decoded tree as indented JSON. Used in a `let` RHS, it
returns the structured value silently.

## file adapters

Many host tools (`diff`, `jq`, `wc`, `sha256sum`) expect inputs as
**file paths** rather than command-line strings. The `file`
adapters bridge REPL values into the filesystem.

### file temp

`file temp VAR` writes the bound value of `VAR` to a private
temporary file and returns the path. Scalar values are written
verbatim; structured values are rendered as indented JSON with a
trailing newline.

```
let data = [json parse '{"b":2,"a":1}']
let path = [file temp data]
```

The temporary file's lifetime is the session; it is cleaned up when
the REPL exits.

### file:$VAR (inline adapter)

In `exec` argument position, `file:$VAR` writes the value to a
single-use temp file and substitutes the path. The temp file is
cleaned up immediately after the `exec` command returns.

```
let a = [exec bpftool map dump id 123]
let b = [exec bpftool map dump id 123]
exec diff file:$a.stdout file:$b.stdout
```

The inline form is the right choice when you need adaptation for a
single invocation. `file temp` is the right choice when you need a
reusable path across multiple commands.

### Path access

- `$p` where `p` was bound by `file temp` resolves to the path
  string.
- `file:$VAR.field` pre-extracts a field from a structured value
  before writing to the temp file.

## Putting it together

A realistic setup-test-cleanup script:

```
# Setup: create a dummy interface.
require ok exec ip link add dummy0 type dummy
require ok exec ip link set dummy0 up

# Load and attach.
let prog = [bpfman load file --path testdata/stats.o --programs xdp:xdp_stats:xdp]
require ok bpfman link attach xdp -i dummy0 $prog

# Exercise.
let stats = [exec bpftool map dump pinned /sys/fs/bpf/bpfman/maps/$prog.record.program_id/stats --json]
let data  = [json parse $stats.stdout]
assert $data[0].packets >= 0

# Cleanup.
bpfman program delete $prog.record.program_id
exec ip link del dummy0
```

## Adding a new builtin

New builtins are a language decision. Before adding one:

1. Is the use case local to a single workflow, or does it recur?
2. Can it be expressed as a bpfman domain command instead? Domain
   commands live under `bpfman`; shell builtins should stay
   general-purpose.
3. Does it follow the existing shape: arguments via ordinary
   expansion, return a `Value` with an explicit origin kind, errors
   surfaced via the standard error channel?

If yes on all three, add the builtin; if no, reconsider.
