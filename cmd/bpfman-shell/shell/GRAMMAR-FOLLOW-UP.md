# bpfman-shell grammar follow-up

This document collects language surprises a reader hits when
learning bpfman-shell, captured during the SCOPE-DESIGN
review pass. It is the companion checklist to `GRAMMAR.md`
(the reference grammar) and `SCOPE-DESIGN.md` (the scope
implementation plan).

The items below are not redesign proposals. Each is one of:

- **Addressed** -- already in `SCOPE-DESIGN.md` or
  `GRAMMAR.md` as committed.
- **Pending** -- worth a follow-up commit; not in the
  ten-commit SCOPE-DESIGN sequence.
- **Accepted** -- acceptable surprise; document loudly and
  do not change the language.

Each item names the surface that surprises a reader, the
existing mitigation if any, and the recommended action.

## 1. `let p <- cmd` discards the envelope (Accepted)

The plain form binds the primary value and discards
`rc.ok`. `guard p <- cmd` halts on non-ok; `let (rc p) <-
cmd` exposes both. The surprise is that plain `let p <- cmd`
reads like "bind successful result", but means "bind primary
and discard status".

This is not a foot-gun in general -- it is the natural
shape for probe commands where non-zero rc is data (`grep`,
`test`, `bpfman program get`). See the bind-form style
guide at the end of this doc for the shape-based rule:
`guard` for required lifecycle, `let (rc x)` for branching
on status, plain `let` for probes.

## 2. Bare commands inside `eventually` are assertions (Addressed)

The same `test -f file` outside `eventually` may be a
side-effect command whose envelope you ignore; inside, an
uncaptured non-ok envelope aborts the attempt and retries.
This is the deliberate "commands are conditions" rule
documented in `GRAMMAR.md` under `EventuallyStmt` and
`SCOPE-DESIGN.md` Section 3.4. Memorable phrasing:

    Inside eventually, bare commands are assertions.

## 3. `assert` inside `eventually` is retryable (Addressed)

`assert $x == 1` outside `eventually` records a session
failure on mismatch. Inside, the same assertion is an
attempt failure that does not increment the session
counter unless the whole construct times out. The
snapshot/reset protocol in `SCOPE-DESIGN.md` Section 3.4.1
keeps the count honest. Memorable phrasing:

    Inside eventually, assertion failures are attempt
    failures, not test failures, unless the whole
    eventually times out.

## 4. `source` exports defs but not aliases (Accepted)

Aliases are session-level declarations elsewhere, but
sourced libraries do not publish them. This is coherent
with "defs are the module API" and mildly surprising.

Style rule: publish via `def`, not `alias`. Aliases are
an interactive-prompt convenience. A library that wants
`pls` visible to importers writes

    def pls() {
        bpfman program list
    }

not

    alias pls = bpfman program list

## 5. Defs declared inside blocks survive the block (Pending)

Variables are block-scoped; defs and aliases are not.
After

    if $debug {
        def dump() { bpfman program list }
    }
    dump

`dump` is callable when `$debug` was true and an
unknown-command error when it was not. Coherent with the
scope model (frames are variable-only), but visually it
looks like everything inside `{ ... }` is local.

Recommended: leave the semantics; add a checker warning
for `def` (and `alias`) declared inside a block. Not a
hard error -- conditional helpers are occasionally useful
-- but the warning surfaces the asymmetry at write time.
Follow-up commit; checker only, no parser change.

## 6. Defs do not capture variables (Accepted)

A def body looks up `$kind` against the call-site frame
stack, not the definition-site environment. This becomes
load-bearing once `source` is module-scoped: a library
def cannot refer to a top-level library `let`, because
that `let` lives in the discarded sub-session vars. The
future nullary-def shape (`def default_kind() { return
xdp }`) is the migration path; until value-returning defs
land, library "constants" are either inlined into each
def body or pulled into the call site.

Library style note worth pinning in `lib.bpfman` once the
SCOPE-DESIGN commits land: do not use `let` at library
top level expecting visibility; defs see only call-site
frames plus their own.

## 7. `_` is ordinary in def params but discard elsewhere (Pending)

Discard semantics for `_`:

    foreach _ in xs { ... }      # discard
    let (_ x) = pair             # discard
    let (_ x) <- cmd             # discard envelope
    let _ = EXPR                 # rejected (parse error)
    def f(_) { ... }             # currently ordinary name

The asymmetry is worth removing. There is no current use
case for `_` as a def parameter (a name nothing references
inside the body); rejecting it makes `_` consistently
discard-like at binding sites and consistently rejected at
def-param position.

Recommended: reject `_` as a def parameter with "def
parameters cannot bind '_'; use a real name or drop the
slot". Single follow-up commit; touches `parseDefParams`
and tests.

## 8. `not-empty` as a bare literal (Accepted)

`not-empty $x` is the unary predicate; bare `not-empty`
parses as a Word literal because the unary only consumes
an operand when one follows. Command-biased convenience
leaking into expression semantics.

Recommended: leave the parser; document the rule that the
unary requires an operand at expression-leading position,
and flag a checker warning if the bare form appears where
the predicate was clearly intended.

## 9. `[` at statement position routes to `CommandStmt` (Pending)

    [1 2 3]

parses as a command named `[` with arguments `1`, `2`,
`3]`, not as an expression statement holding a list
literal. Parser accident, not an intentional shape, and
exactly the sort of permissiveness that becomes
compatibility debt later.

Recommended: either route `[` at statement position to
`ExprStmt` so `[1 2 3]` becomes a no-op expression
statement, or reject outright with "list literal at
statement position is not allowed". Cheap either way --
the current corpus has no statement-leading `[`. Lean
towards rejection unless an expression-statement use case
appears.

## 10. Command head is statically a Word (Accepted)

`print ($x + 1)` works; `($cmd) arg` does not. Commands
are statically named; only arguments accept rich
expressions.

Memorable phrasing:

    Commands are statically named. Expressions produce
    arguments, not command names.

This fits the SANS-IO / argv-first model and is unlikely
to change. Document it under `CommandStmt`.

## 11. No command substitution despite shell heritage (Accepted)

    print [bpfman program list]

is a list literal, not a command substitution. The only
command-capture form is `<-`. Memorable phrasing:

    <- is the only command-capture form. [...] is a list.

This is a recurring surprise for users coming from POSIX
shell. Keep prominent in the docs.

## 12. Bind-collect foreach collects command primaries only (Accepted)

    guard xs <- foreach x in $items {
        let y = ($x |> jq ".id")
        print $y
    }

`xs` is the list of primaries from the final `print`
(empty strings), not the list of `$y` values. To collect
a computed value, the last statement must be a command
whose primary is the desired value.

Recommended phrasing:

    Bind-collect foreach collects command primaries, not
    arbitrary expression values. Future value-returning
    blocks (SCOPE-DESIGN Section 9) close this gap.

## 13. `return` plus def-local defer failure (Addressed)

Value-returning defs landed in a follow-up to SCOPE-DESIGN.
`let p <- f` with a def whose cleanup defer failed binds
`p` to the returned value and discards the cleanup-failure
envelope, exactly as the bind-form style guide describes.
`let (rc p) <- f` exposes the cleanup outcome through
`$rc.ok`; `guard p <- f` halts via GuardFailure on a flipped
envelope. See `GRAMMAR.md` ReturnStmt and SCOPE-DESIGN.md
Section 9 for the contract.

## What to change next

The Pending items, in rough priority:

1. Reject `_` as a def parameter (item 7). Single parser
   change, no corpus impact.
2. Reject or reroute `[` at statement position (item 9).
   Single parser change, no corpus impact.
3. Checker warning for `def` / `alias` declared inside a
   block (item 5). Pure diagnostic, no semantic change.

Items 1 and 2 are single-commit parser changes and could
land alongside SCOPE-DESIGN commit 9 (corpus migration)
or as a separate small PR. Item 3 is a checker addition
that fits naturally on top of SCOPE-DESIGN commit 2
(checker frames).

The remaining Accepted items are documentation work,
covered by `GRAMMAR.md` and `SCOPE-DESIGN.md` once the
implementation sequence completes.

## Bind-form style guide

The biggest user-facing surface is the bind family. The
right form depends on what non-ok rc means for the
script:

- `guard x <- cmd` -- non-ok rc cannot continue. Use for
  required lifecycle steps: program load, link attach,
  dispatcher configure, anything where failure means the
  script's preconditions are violated.

- `let (rc x) <- cmd` -- non-ok rc is meaningful and the
  caller branches on it. Use when both the status and the
  primary matter, especially for commands whose envelope
  carries diagnostic data (stdout / stderr).

- `let x <- cmd` -- non-ok rc is data, not failure. Use
  for probing commands and "is this thing currently true"
  checks where the status is part of the answer:

      let found   <- grep pattern file
      let exists  <- test -f path
      let present <- bpfman program get $pid

  Single-bind discards the envelope intentionally. Use
  this form when the rc carries no information the caller
  needs.

Worked contrast:

    # required: halt if the load fails
    guard p <- bpfman program load file --path foo.o
    defer bpfman program unload $p

    # probe: rc is the question being asked
    let found <- grep pattern file
    if $found.ok { print "matched" } else { print "no match" }

    # mixed: branch on rc, then use the primary
    let (rc out) <- grep pattern file
    if $rc.ok { print $out }

The rule is shape-based, not avoidance-based:

- lifecycle code: `guard` unless you can name a specific
  reason not to;
- probe / check / interrogation code: `let`, because the
  rc is the answer;
- branching code: `let (rc x)`, because both halves of the
  envelope matter.

Avoid plain `let p <- cmd` only when the command's non-ok
rc would mean "broken precondition" but the script does
not check it. That is the genuine foot-gun, and a checker
warning that fires on `let p <- bpfman ...` (lifecycle
shape) without a subsequent `$p.ok` inspection might be
worth adding later. The same warning would not fire on
`let found <- grep ...` because `grep` is a probe whose
non-ok rc is meaningful by design.
