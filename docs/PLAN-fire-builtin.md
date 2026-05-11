# Plan: the `fire` built-in for e2e kernel-stimulus fixtures

## 1. What this document is

A working design memo for `fire`, a new effectful built-in in the
bpfman-shell DSL. Retire this file once the built-in ships and its
user-facing surface lands in `REPL-REDESIGN.md`; the implementation
will live in the code.

## 2. The current ceremony

The e2e/new corpus spawns deterministic kernel-stimulus workers using
the env-var dispatch already wired into `cmd/bpfman-shell/main.go`:

```
guard work <- start env BPFMAN_SHELL_MODE=unlinkat-fire-worker \
    bpfman-shell $sentinel $ack $n 1
```

Read at the call site, this is four tokens of mechanism:

- `start` -- the lifecycle verb (a real spawn);
- `env BPFMAN_SHELL_MODE=unlinkat-fire-worker` -- the env-var
  encoding of the worker's personality;
- `bpfman-shell` -- the binary that hosts that personality;
- positional `$sentinel $ack $n 1` -- the wave-protocol parameters.

Only the last bullet reflects what the *test* cares about. The first
three are layer-3 implementation choices (env var as dispatch,
single-binary helper, exec path) leaking into the layer-4 script.

The mode name (`unlinkat-fire-worker`) is itself the verb the script
wants to say. Today the language forces it to be a string inside an
env var.

## 3. Proposed surface

A single effectful built-in named `fire`, taking a *kind* as the
first argument and a `--count` / `--waves` protocol pair as flags.
Sentinel and ack are positional (their order is obvious from
adjacent `let` bindings).

```
guard work <- fire unlinkat $sentinel $ack --count=$n --waves=1
guard work <- fire kill     $sentinel $ack --count=$n --waves=1
guard work <- fire uprobe   $sentinel $ack --count=$n --waves=1
```

`fire` returns a `Job` primary, the same shape `start` produces. From
the script's point of view the worker is a managed background
process; `wait`, `kill`, and `defer kill $work` all compose
unchanged:

```
guard work <- fire unlinkat $sentinel $ack --count=$n --waves=1
defer kill --signal=KILL $work
guard loaded <- bpfman program load file \
    --path testdata/bpf/kprobe_exact.bpf.o \
    --programs kprobe:kprobe_counter \
    -g "expected_pid=0x${u32le $work.pid}" \
    -g "weight=0x${u64le $weight}"
guard link <- bpfman link attach kprobe --fn-name do_unlinkat $loaded.programs[0]
guard _ <- touch "${sentinel}.1"
guard rc <- wait $work
assert $rc.ok
```

`fire` is not a new lifecycle model. It is a domain-specific
constructor for a `Job` whose target happens to be a co-resident
bpfman-shell helper. The "configure now, attach later, trigger
later, observe later" staging that already underpins `start` /
`wait` is preserved.

## 4. Architectural framing

The split is the deep-module move: the script chooses the kind and
the protocol parameters; the built-in owns binary resolution,
env-var construction, and process shaping.

```
  start         low-level process primitive (any binary, any env)
  fire <kind>   high-level e2e fixture primitive (kernel-stimulus workers)
```

`fire` is sugar over `start`, not a parallel mechanism. Internally
it resolves `/proc/self/exe`, synthesises
`BPFMAN_SHELL_MODE=<kind>-fire-worker`, and delegates to the same
machinery `start` already calls. The `start env
BPFMAN_SHELL_MODE=...` spelling stays valid as a debug escape hatch:
when something breaks in CI it can be pasted into a terminal and
bisected by hand.

The category in the registry is *fixture builtin returning a Job*,
not *pure builtin* (no side effects) and not a hypothetical *task
builtin* (in-process goroutine). These workers are deliberately
process-shaped:

- stable PID for BPF program filtering (`expected_pid`);
- ELF path for uprobe attachment;
- process-group cleanup;
- real kernel syscalls, not Go-runtime simulations;
- exact process lifecycle observable via `wait`.

## 5. Declarative shape

`fire` makes the e2e scripts more declarative about causality. The
script states the testing intent:

- construct a deterministic kernel-stimulus producer;
- load and attach the BPF program;
- release a named wave;
- wait for the producer's completion barrier;
- assert the observed map / link / program state.

The script no longer spells the worker launch mechanism at the call
site. The env var, helper mode, binary lookup, and process-shaping
details are implementation machinery owned by `fire`.

This turns:

```
guard work <- start env BPFMAN_SHELL_MODE=unlinkat-fire-worker \
    bpfman-shell $sentinel $ack $n 1
```

into:

```
guard work <- fire unlinkat $sentinel $ack --count=$n --waves=1
```

The second form says what the test needs: "fire unlinkat events".
The script reads as a sequence of causal test steps rather than a
recipe for invoking a helper process.

## 6. The `Job` shape and `target_binary`

`fire`-produced Jobs publish a `target_binary` field carrying the
absolute path of the running bpfman-shell ELF. Uprobe scripts read
it directly:

```
guard link <- bpfman link attach uprobe \
    --target $work.target_binary \
    -f bpfman_shell_uprobe_call_malloc \
    $prog
```

This replaces the `command -v bpfman-shell` round-trip in today's
uprobe scripts and strengthens the binary-vs-worker correspondence:
the script cannot accidentally attach to a different binary than the
worker is running in.

`target_binary` is populated by two paths with different semantic
weights. `fire` guarantees the field is the running bpfman-shell
ELF: the exact image whose symbols a uprobe attach may target, the
stable test-fixture identity the script relies on. Plain `start`
populates `target_binary` from the executable it launched, as
best-effort identity. Only `fire` kinds with `NeedsBinary == true`
carry the field's semantic contract, and only those accesses
receive check-time guarantees; plain-`start` jobs do not.

### Constraint on the field's growth

`target_binary` is *not* a general process-inspection surface. It is
part of the observable kernel-attachment surface; it is the stable
test-fixture identity; it is required for uprobe correctness. The
field's documentation must say so explicitly, because the natural
follow-up requests ("expose `cwd`", "expose the inode", "expose
namespaces", "expose argv0") all land `fire`-produced Jobs on the
path back to a tiny process supervisor.

Indicative wording, to live at the field's declaration site:

```
// target_binary is populated when the launched job corresponds to
// a stable executable image that kernel attachment APIs may target
// (uprobes, symbol resolution, etc.). It is not intended as a
// general process-inspection surface.
```

## 7. Registry shape

Each fire kind registers itself from its helper file's `init`
function into a small map. The kind name is the script-facing token;
the entry value carries the env-mode string and a short semantic
description:

```
type fireKind struct {
    Mode        string  // BPFMAN_SHELL_MODE value to set in the child's env
    Summary     string  // one-line description for help / completion
    NeedsBinary bool    // true if the kind requires `Job.target_binary`
}

var fireKinds = map[string]fireKind{}

func registerFireKind(name string, k fireKind) { ... }
```

`unlink_helper.go`, `kill_helper.go`, and `uprobe_helper.go` each add
a single init line registering their kind alongside the existing
`BPFMAN_SHELL_MODE` switch entry. Adding a kind is one init line
plus one switch arm in `main.go:runMode`; removing a kind is the
inverse. The `runMode` switch is the authoritative dispatch; the
registry is the script-facing index.

The three fields earn their place:

- `Mode` is the implementation glue (registry -> env var).
- `Summary` seeds help / completion text without requiring an
  out-of-band documentation file.
- `NeedsBinary` enables check-time validation: a script that writes
  `$work.target_binary` against a job whose producing kind has
  `NeedsBinary == false` (or against a generic `start`'d job that
  happens to have no meaningful target) can be refused at parse,
  rather than failing at runtime with an empty string.

No fourth field today. A `Tracepoints []string` documenting which
kernel hooks each kind targets is tempting for help output but is
not load-bearing; it can land if a future `help fire` materialises.

## 8. Naming

Kinds are named after the kernel boundary the worker exercises:

- `unlinkat` -- fires `unlinkat(2)`, observed at `do_unlinkat` and
  `sys_*_unlinkat`;
- `kill` -- fires `kill(2)`, observed at `syscalls/sys_enter_kill`;
- `uprobe` -- fires the cgo'd target symbol
  `bpfman_shell_uprobe_call_malloc`, observed by attached uprobes /
  uretprobes.

The `kill` kind name lexically collides with the existing `kill`
built-in (which sends a signal to a job). The collision is
contextual not syntactic: `fire kill $sentinel ...` and `kill $work`
never occupy the same parse position, and a human reader sees `fire
kill` as a two-token verb and `kill` alone as a one-token verb
taking a job handle. The collision is the same shape as English
`time` (noun) vs `time` (verb) sharing a spelling.

Preserving the "kind = syscall fired" principle across all three
kinds is more valuable than dodging the collision by renaming to
`signal-self` or `kill-self`. A mixed naming rule (some kinds after
the syscall, some after the semantic effect) forces a coin-flip per
future kind. If the muddiness ever bites in practice, the
disambiguating rename is `kill-syscall`, which preserves the
principle.

## 9. Scope boundary

`fire` is for syscall-, signal-, and uprobe-shaped kernel stimulus
generators. It is *not* a generic process launcher, a network
topology builder, a fake-service framework, or a synthetic
Kubernetes harness. Those would all be candidates for an arbitrary
daemon-launching surface; `fire` deliberately is not that.

A one-line scope comment at the registry declaration site captures
this so a future contributor reaching for "while we're here, let's
add `fire envoy`" sees the boundary at the registration point:

```
// fire is for syscall / signal / uprobe event generators only.
// A richer fixture surface, if needed, lives in its own subsystem,
// not in this registry.
```

If the registry grows beyond a handful of kinds, that is evidence
the fixture domain itself wants its own runner or DSL. Not a problem
to solve today.

## 10. Validation timing

v1: runtime validation in the `fire` handler. Unknown kind ->
runtime error; missing flags -> runtime error.
`$work.target_binary` is present only when populated; accessing it
on a Job without a target binary is a runtime field error, not a
silent empty string. An empty string would flow into
`bpfman link attach uprobe --target ""` undetected; a field error
fails the script at the offending line.

v2 (optional, defer): hoist the kind name and `NeedsBinary` through
to the check-time validator so a typo'd kind and an incoherent
field access both fail at parse. This mirrors the pure-builtin
registry's check-time pattern but does not need to land in the
first cut.

## 11. Migration plan

Three commits, mirroring the shape of the recent pure-builtin arc
(infrastructure -> handler -> corpus sweep):

1. *Built-in and registry:* add the effectful built-in `fire` and
   the kind registry; register `unlinkat`, `kill`, `uprobe` from
   their helper files' init. Add `target_binary` to the `Job` shape
   and populate it from `/proc/self/exe` in the fire handler (and
   from the `start` arg in the existing `start` path).

2. *Sweep e2e/new:* mechanical substitution across all
   `Test*_LoadAttachDetachUnload.bpfman` scripts. The env-var
   spelling collapses into `fire <kind>`; the four uprobe sites
   drop the `command -v bpfman-shell` exec and read
   `$work.target_binary` instead. Expected diff: ~50 lines removed,
   no behavioural change.

3. *Check-time validation (deferred):* validate kind name and
   `target_binary` access at parse. May be folded into the first
   commit if it falls out cheaply; otherwise lands later.

The `start env BPFMAN_SHELL_MODE=...` spelling and the
`cmd/bpfman-shell/main.go:runMode` switch stay unchanged at every
step. The escape hatch is preserved by construction.

## 12. Alternatives considered

### A CLI subcommand on bpfman-shell

`bpfman-shell fixture <kind>` would let the script read

```
guard work <- start bpfman-shell fixture unlinkat $sentinel $ack $n 1
```

This cleans up the worst leak (env-var encoding) without growing the
shell-language surface. It is rejected here because the ceremony at
the call site is still `start ... bpfman-shell`; the script still
names the binary explicitly and the verb the script wants (`fire
unlinkat`) does not appear at the call position. The subcommand is
a *spelling* improvement; `fire` is an *abstraction* improvement.

### A user-level `def` in `lib.bpfman`

Most attractive on paper: zero language change, opt-in via `source
lib.bpfman`, fixture vocabulary stays out of the global namespace.
Closed off by a real language limit: `callDef` in `shell/expr.go`
returns `error`, never a `Value`; a `def` body is a statement
sequence with no return-value contract. A `def fire-unlinkat(...)`
cannot capture and return the `Job` that `start` produces inside it.

The gap is worth flagging separately: defs propagating a return
value would unlock more than this case (parsing helpers, JSON
projection helpers, small composed-command wrappers). That is a
larger language change and is not on this plan's critical path.

### Three sibling built-ins (`fire-unlinkat`, `fire-kill`, `fire-uprobe`)

Considered and rejected. A single `fire` with a kind argument keeps
the registry small, makes adding a kind a registration line instead
of a grammar change, and reads as a deliberate dispatch ("fire a
kind of stimulus") rather than three near-duplicate verbs.

### An in-process worker goroutine

Rejected because the workers must be process-shaped: stable PID for
BPF filtering, real `/proc/self/exe` for uprobe attachment,
process-group cleanup, real syscall trap points the kernel can
observe. An in-process goroutine has none of these properties.
