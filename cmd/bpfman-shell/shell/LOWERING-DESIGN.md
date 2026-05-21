# bpfman-shell lowering design

**Status.** We are going to define a lowered IR for
bpfman-shell, dump it as text, snapshot it against the script
corpus, and execute it. The lowered form is the canonical
semantic representation of a program. The existing AST tree
walker may remain during bring-up, but only as migration
scaffolding; it is not the intended steady state.

This document is the companion to `GRAMMAR.md` (surface
grammar), `SCOPE-DESIGN.md` (runtime scope / defer / eventually
semantics), and `LANGUAGE-DIRECTION.md` (what the language is
trying to become). It answers a narrower question than those
docs:

    What semantic picture do we want on disk, and how do we make
    it the thing the shell actually runs?

## Why lower now

The current AST dump is useful, but it shows syntax, not
semantics. It answers:

- how the parser grouped an expression;
- whether a command became a `BindStmt`, `CommandStmt`, or
  `EventuallyStmt`;
- where tokens and spans landed.

It does **not** answer the questions the recent bug stream kept
raising:

- where frames open and close;
- where defer scopes open and unwind;
- where a `return` is stashed before cleanup;
- where `guard` halts versus where plain `let` continues;
- where `eventually` retries, times out, or fails fatally;
- where the bind-family `Rc` / `Primary` split is applied.

Those are semantic-shape bugs, not parse-shape bugs. Reading the
current evaluator means mentally executing a few hundred lines of
Go across `expr.go`, `check.go`, and the cmd-side dispatch
bridges. A readable lowered form turns that into a file we can
inspect, diff, grep, review, and execute.

The key architectural point is simple:

    parse -> check -> lower -> execute lowered form

If the lowered form is the semantic picture we trust, it should
also be the representation the interpreter runs. Otherwise we
keep two semantic engines:

- the lowered form that claims to show program intent; and
- the tree walker that actually decides behaviour.

That split will drift. The dump becomes advisory instead of
authoritative, which weakens the whole exercise.

## Target architecture

The lowered IR is the canonical semantic form of a program.
Execution, debugging, and semantic regression testing all revolve
around it.

That means:

- lowering is not a side artefact;
- the dumper is a view of the executed program, not an advisory
  sketch;
- the interpreter consumes lowered blocks/instructions rather than
  re-walking the AST;
- the AST tree walker is temporary migration scaffolding and
  should be deletable once parity is achieved.

The migration can still be staged. The first milestone is a
readable dump plus corpus snapshots, because that makes the IR
legible before we trust it at runtime. But the endpoint is not
"keep both paths forever". The endpoint is one semantic engine.

## Migration scope

The migration has two coupled outputs:

1. a readable lowered IR with a stable dump format; and
2. an interpreter that executes that lowered IR.

Deliverables:

1. A readable lowered IR in `cmd/bpfman-shell/shell/`.
2. `Lower(*Program) (*LoweredProgram, error)` or equivalent API.
3. `DumpLowered(io.Writer, *LoweredProgram)` text dumper.
4. A CLI mode that mirrors `--ast`, tentatively
   `bpfman-shell --lowered FILE`.
5. Golden-file tests over the script corpus.
6. An interpreter entry point that executes lowered programs.
7. A migration plan for deleting the AST tree-walk execution path
   once parity is good enough.

Non-deliverables:

- no bytecode;
- no optimizer;
- no user-visible language changes;
- no attempt to lower the entire expression language into a new
  mini-VM on day one.

The important bound is sequencing, not destination: we still want
the dump and snapshots early, but they are the on-ramp to
execution rather than the endpoint.

## Goals

### 1. Legibility first

The lowered dump is for humans before machines. A reviewer should
be able to read it top-to-bottom and answer "what does this
program mean?" without reading evaluator internals.

### 2. One semantic engine

The long-term runtime should not keep both a lowered semantic form
and a separate AST evaluator. The lowered form is supposed to be
the semantic truth, so it must be the thing we execute.

### 3. Semantic, not syntactic

The IR should make hidden runtime structure explicit:

- frame lifetime;
- defer-scope lifetime;
- dispatch kind;
- bind policy;
- `eventually` attempt structure;
- return stashing and unwind order.

It should **not** mirror the AST one-for-one or it buys little
over `DumpAST`.

### 4. Deterministic output

The dump must be stable enough for golden files:

- deterministic block labels;
- deterministic temp names;
- deterministic instruction ordering;
- no pointer addresses or map-order noise;
- spans printed in a consistent shorthand.

### 5. Bounded migration

The migration should be staged so we can validate the IR before
cutting execution over completely:

- define the IR and dumper first;
- snapshot it against the corpus;
- bring up interpretation with parity tests;
- delete the old path once confidence is high.

## Non-goals

### Not a bytecode design

The first IR is not about flattening into an execution format.
It may be block-structured and may use richer instruction forms
than a bytecode VM would want. Readability wins.

### Not an optimizer

We are not folding constants, removing dead blocks, or
normalizing expressions for speed. Any optimization pressure is a
later concern.

### Not a full expression compiler

The first interpreter may keep expression subtrees embedded in
instructions such as `EvalExpr` or `BuildArgs`. The win we want
now is around statement/control/bind/defer structure, not around
lowering every `$x + 1` into register-level ops.

### Not two permanent runtimes

During migration there may briefly be both:

- a tree-walk evaluator; and
- a lowered-form interpreter.

That overlap is transitional only. It is explicitly **not** a
goal to keep both semantic engines alive long-term.

### Not a replacement for helper cleanup

Canonical envelope constructors, canonical dispatch helpers, and
matrix tests still pull their weight. Lowering does not make
those refactors a mistake; it makes them implementation details
of one execution path instead of scattered policy across several.

## The IR we want

The lowered form should be **block-based** and **explicit**.
Think "structured control-flow listing", not "opaque opcode
stream".

It must serve two readers:

- humans inspecting the dump; and
- the interpreter executing the program.

Those goals are compatible as long as the first version optimises
for semantic clarity over machine compactness.

### Core entities

- `LoweredProgram`
- `LoweredDef`
- `BasicBlock`
- `Instr`
- `Temp`
- `Label`

Every instruction carries a source span (or a pointer to one) so
the dump can still be correlated with the original script and the
interpreter does not lose diagnostic fidelity.

### Core instruction families

These are the instruction families the lowerer and interpreter
should share. The spelling is illustrative; the exact Go type
names can change.

#### Scope and lifetime

- `EnterFrame kind=<def|if-branch|foreach-iter|eventually-attempt|source-module>`
- `ExitFrame`
- `EnterDeferScope kind=<program|def|eventually-attempt|source-file>`
- `RegisterDefer argv=[...]`
- `RunDefers policy=<program|def-local|attempt-fatal>`

#### Evaluation and binding

- `EvalExpr dst=tN expr=<Expr>`
- `BuildArgs dst=tN args=<[]ArgExpr>`
- `DispatchBind dst=tN argv=tArgs`
- `DispatchCommand argv=tArgs`
- `ApplyBind src=tResult primary=<name|_> rc=<name|_> guard=<bool>`
- `BindName name=<ident> src=tN`

#### Result construction and termination

- `BuildEnvelope dst=tN ok=<bool> code=<int> err=<string|nil>`
- `BuildEventuallyResult dst=tN ok=<bool> timed_out=<bool> ...`
- `EmitBindResult rc=<temp|synthetic> primary=<temp|nil>`
- `Stop`

#### Control flow

- `Jump bbN`
- `Branch cond=tN true=bbX false=bbY`
- `ReturnValue src=tN`
- `PropagateError`
- `Fail span=<...> msg=<...>` for structural lowering-time
  surfaces that become explicit control-flow exits

#### Eventually

`eventually` is important enough that the IR should make its
attempt structure obvious rather than encoding it as hidden
evaluator recursion.

The first iteration can choose either of two readable shapes:

1. A small family of explicit attempt instructions:
   `BeginEventually`, `BeginAttempt`, `RetryIfRetryable`,
   `EventuallyTimeout`, `EventuallySuccess`.
2. A structured pseudo-instruction with labelled regions:
   `Eventually timeout=... interval=... attempt=bbX timeout=bbY success=bbZ`.

Either is acceptable. The rule is readability:

    a human must be able to see where an attempt begins, where
    defers unwind, and which path is retryable versus fatal.

### What stays embedded in the first iteration

The following may remain embedded rather than recursively lowered
in the first interpreter:

- expression trees on `EvalExpr`;
- command-argument trees on `BuildArgs`;
- the exact payload of record/list literals.

This is deliberate. The first-value picture is about control and
resource semantics, not about replacing the expression evaluator.

## Example dumps

The final concrete syntax of the dump can evolve, but it should
aim for this level of explicitness.

### Example 1: def with return and cleanup

Source:

```bpfman
def load_prog(path) {
  guard prog <- bpfman program load file --path $path
  defer bpfman program unload $prog
  return $prog
}
```

Illustrative lowered dump:

```text
def load_prog(path) entry=bb0

bb0:
  EnterFrame kind=def
  EnterDeferScope kind=def
  BuildArgs t0 = ["bpfman", "program", "load", "file", "--path", $path]
  DispatchBind t1 = t0
  ApplyBind src=t1 primary=prog rc=_ guard=true fail=bb_fail
  BuildArgs t2 = ["bpfman", "program", "unload", $prog]
  RegisterDefer argv=t2
  EvalExpr t3 = $prog
  ReturnValue t3 -> bb_return

bb_return:
  RunDefers policy=def-local
  ExitFrame
  EmitBindResult rc=synthetic primary=t3

bb_fail:
  RunDefers policy=def-local
  ExitFrame
  PropagateError
```

The point is not the exact instruction names; the point is that
the return-stash-before-cleanup order is visible on disk and is
the order the interpreter will run.

### Example 2: eventually bind

Source:

```bpfman
let (rc r) <- eventually timeout 1s interval 50ms {
  guard _ <- exec false
}
```

Illustrative lowered dump:

```text
bb0:
  BeginEventually timeout=1s interval=50ms retry=bb_attempt timeout=bb_timeout success=bb_done

bb_attempt:
  EnterFrame kind=eventually-attempt
  EnterDeferScope kind=eventually-attempt
  BuildArgs t0 = ["exec", "false"]
  DispatchBind t1 = t0
  ApplyBind src=t1 primary=_ rc=_ guard=true fail=bb_retry
  RunDefers policy=attempt-fatal
  ExitFrame
  Jump bb_done

bb_retry:
  RunDefers policy=attempt-fatal
  ExitFrame
  RetryIfRetryable last=t1 else=bb_fatal

bb_timeout:
  BuildEventuallyResult t2 = { ok=false, timed_out=true, ... }
  BuildEnvelope t3 = { ok=false, code=1, err="eventually timed out" }
  BindName rc = t3
  BindName r = t2
  Stop
```

Again, the important property is not compactness; it is that the
retry and bind semantics are visually obvious and runtime-visible
in one place.

## Dump format rules

The dump should obey a few strict formatting rules so snapshots
stay readable and stable.

### Deterministic naming

- blocks named `bb0`, `bb1`, ... in first-emission order;
- defs printed in source order;
- temps named `t0`, `t1`, ... in first-definition order inside
  one lowered unit;
- labels never include pointer-like IDs.

### Stable rendering

- one instruction per line;
- spans rendered with the same `line:col-line:col` shorthand the
  AST dump uses today;
- embedded expressions rendered with a compact, deterministic
  printer;
- lists and maps in deterministic field/key order.

### Human-first block boundaries

- blank line between blocks;
- block labels flush left;
- instruction operands use named fields when position alone would
  be ambiguous.

## Golden-file plan

This is the part that pays for the whole exercise quickly.

### Corpus snapshots

Every corpus script becomes a checked-in lowered snapshot. The
initial scope is:

- `e2e/scripts/**/*.bpfman`
- `e2e/new/**/*.bpfman`
- `e2e/lib.bpfman`

Suggested layout:

```text
cmd/bpfman-shell/shell/testdata/lowered/
  e2e/scripts/...
  e2e/new/...
  e2e/lib.bpfman.lowered
```

The test:

1. reads the source script;
2. tokenises and parses it;
3. lowers it;
4. dumps the lowered form;
5. compares against the checked-in golden file.

This gives us semantic regression testing at the text-file
level. Grammar or lowering changes no longer ask only "did the
runtime tests still pass?" They ask "which semantic pictures
moved, and why?"

### Focused unit fixtures

The corpus is broad, but some semantic corners deserve tiny
purpose-built snippets too:

- `return` plus def-local defer;
- `guard <- eventually`;
- bind-collect producer dispatch;
- sourced-file module boundaries;
- conditional def rejection sites.

Those focused fixtures live alongside the corpus snapshots so the
golden discipline applies to both real scripts and surgical edge
cases.

## CLI and API surface

The dump mode should mirror the existing AST flow closely so it
feels like a sibling tool rather than a new subsystem.

### Package API

- `Lower(prog *Program) (*LoweredProgram, error)`
- `DumpLowered(w io.Writer, lp *LoweredProgram) error`
- `ExecLowered(env Env, prog *LoweredProgram) error` or
  equivalent interpreter entry point

### CLI

Add a mode parallel to `--ast`:

```text
bpfman-shell --lowered FILE
```

The mode should:

- slurp the whole file as one program, like `--ast`;
- parse it;
- lower it;
- dump the IR to stdout;
- exit non-zero on tokenise/parse/lower/dump failure.

The normal execution path should lower first and then execute the
lowered program.

### REPL builtin

The existing `ast` builtin makes interactive inspection easy.
There should be a sibling builtin for lowered output, but it is
acceptable to land the CLI mode first and the REPL builtin in the
next patch if that keeps the first slice small.

## Implementation order

This sequence keeps the migration bounded without blurring the
target architecture.

### 1. Define the IR for readability

Write the Go types and dumper first. This is the stage where the
shape is still optimised for human legibility.

### 2. Lower AST to IR

Implement statement/control lowering, leaving expression trees
embedded where useful. Get the dump legible before making the
interpreter complete.

### 3. Add dump mode

Make the IR observable from the CLI and, if cheap, the REPL.

### 4. Snapshot the corpus

Add golden files and tests. From that point on, grammar and
lowering changes get semantic diffs in review.

### 5. Bring up interpretation

Execute lowered programs behind tests, compare behaviour with the
existing runtime, and close parity gaps while the dump remains the
primary inspection tool.

### 6. Cut execution over

Make the lowered interpreter the default runtime path.

### 7. Delete the AST tree walker

Once parity and confidence are good enough, remove the old
execution path so the language has one semantic engine again.

## Parity and cutover

The trigger for **defining** the IR is observability. The trigger
for **cutting execution over** is stricter: the lowered
interpreter must demonstrate that it preserves the current
language contract.

That means:

- corpus scripts still pass;
- focused semantic fixtures still pass;
- bind-family result shapes match documented behaviour;
- defer/unwind ordering matches the design docs;
- checker/runtime mismatches do not increase during migration.

The dump and its golden files are part of that parity story. They
do not replace runtime tests, but they make semantic drift
reviewable.

## Open questions

### 1. How flat should `eventually` be?

Readable structured pseudo-instructions may be better than a
fully flattened CFG in the first interpreter. The lowerer’s first
encounter with `guard <- eventually` should decide this with a
real example, not by taste.

### 2. How much of expressions do we lower?

Keeping expressions embedded is the bounded move. If expression
shape becomes the next debugging pain, a later phase can lower
them further.

### 3. Corpus snapshots or focused fixtures first?

The answer is both: snapshot the whole corpus so real scripts
become semantic artefacts, and keep a small set of focused
fixtures for edge cases whose shape deserves surgical review.

## Decision

We are proceeding with a lowered IR as the canonical semantic form
of a program, and we intend to execute it.

The first success condition is still observational:

    a reviewer can open a lowered snapshot and understand the
    program's semantic structure faster than they could by reading
    evaluator code.

But that is not the endpoint. The architectural success
condition is:

    the shell executes the same lowered form it dumps, and the old
    tree-walk evaluator can be deleted.

If both happen, lowering has paid off twice: once as a semantic
picture, and once as the runtime we actually trust.
