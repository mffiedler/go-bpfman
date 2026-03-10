# Dispatcher rework: make dispatcher handling a deep module

This document proposes a redesign of dispatcher handling so that the
dispatcher becomes a deep module rather than a cross-cutting protocol
spread across the manager, store, and kernel adapter.

The goal is not to redesign the whole platform layer. Most existing
interfaces outside dispatcher handling are broadly sound. The problem
is localised: dispatcher persistence and dispatcher rebuild currently
leak too much representation and sequencing into their callers.

This rework narrows that leak.

## Executive summary

Today, dispatcher handling is expressed as a set of low-level
operations:

- save a dispatcher row
- increment its revision
- count links for a dispatcher
- list slots for a dispatcher
- delete dispatcher link detail rows
- save individual extension links

Those operations expose the internal SQLite representation and force
the manager to understand dispatcher rebuild mechanics in too much
detail.

That is the architectural defect.

A dispatcher is not fundamentally:

- a row in `dispatchers`
- plus some rows in `links`
- plus some rows in `link_xdp_details` or `link_tc_details`

A dispatcher is fundamentally:

- a stable logical attach point
- with a current revision
- and a current membership snapshot

This document therefore proposes that dispatcher persistence become
snapshot-based rather than row-fragment-based.

The store should expose operations such as:

- get current dispatcher snapshot
- replace current dispatcher snapshot atomically
- delete current dispatcher snapshot atomically

and stop exposing implementation-shaped helpers such as:

- `IncrementRevision`
- `CountDispatcherLinks`
- `ListDispatcherSlots`
- `DeleteDispatcherLinkDetails`

Those helpers are symptoms of a shallow module.

## Problem statement

### 1. Dispatcher handling is represented as mechanics, not semantics

The current `DispatcherStore` interface is:

```go
type DispatcherStore interface {
    GetDispatcher(ctx context.Context, dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) (dispatcher.State, error)
    ListDispatchers(ctx context.Context) ([]dispatcher.State, error)
    SaveDispatcher(ctx context.Context, state dispatcher.State) error
    DeleteDispatcher(ctx context.Context, dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) error
    IncrementRevision(ctx context.Context, dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) (uint32, error)
    CountDispatcherLinks(ctx context.Context, dispatcherProgramID kernel.ProgramID) (int, error)
    ListDispatcherSlots(ctx context.Context, dispatcherProgramID kernel.ProgramID) ([]DispatcherSlot, error)
    DeleteDispatcherLinkDetails(ctx context.Context, dispatcherProgramID kernel.ProgramID) error
}
```

This is not a semantic dispatcher API. It is a set of persistence
primitives tied to the current schema layout.

It forces callers to know:

- that a dispatcher has a separately stored revision
- that extension links are stored independently
- that old detail rows must sometimes be removed explicitly
- that slot reconstruction requires joining across links, details,
  and programs
- that emptiness is detected by counting extension rows

That knowledge should be buried.

### 2. DeleteDispatcherLinkDetails is not a real domain operation

`DeleteDispatcherLinkDetails` is the clearest architectural smell.

It encodes an operation that exists only because the current
representation is fragmented:

- delete the XDP/TC detail rows
- but leave the base `links` rows alone

That is not a meaningful dispatcher lifecycle event. It is a partial
teardown of the current schema representation. It creates or
preserves broken intermediate states and requires callers to know how
to repair them later.

A caller should never need to express this.

### 3. Revision handling leaks out of the module

`IncrementRevision` is another leak.

Revision is not a free-standing business operation. It is part of a
dispatcher state transition: a new current snapshot replaces the old
one.

Exposing revision bumping as a public primitive encourages multi-step
protocols of the form:

1. increment revision
2. save dispatcher
3. delete some old rows
4. insert some new rows

That is exactly the kind of sequencing complexity a deep module is
meant to hide.

### 4. CountDispatcherLinks and ListDispatcherSlots are reconstruction helpers

These methods exist because the current store API does not expose the
actual thing the caller wants: the current dispatcher membership
snapshot.

`CountDispatcherLinks` asks the wrong question at the wrong layer.
Callers do not want "count rows by dispatcher program ID". They want
"what is the current member set of this logical dispatcher?" A count
is a lossy derivative of a snapshot. Exposing it forces callers to
reason about row cardinality when they should be reasoning about
membership.

`ListDispatcherSlots` is similar: it returns a persistence-shaped
reconstruction blob that callers must stitch together with the
dispatcher row, pinned program paths, and link IDs to recover what
should have been returned as a single snapshot.

This reconstruction belongs inside the dispatcher module.

### 5. SaveLink is overloaded across two different models

For most link kinds, `SaveLink` is appropriate. A tracepoint link or
kprobe link is naturally persisted as one link record plus one detail
record.

Dispatcher extension links are different.

Persisting the extension set for a dispatcher is not conceptually a
sequence of independent `SaveLink` calls. It is a replace-the-world
snapshot update:

- there is one logical dispatcher
- it has one current revision
- that revision has one complete current member set

Treating that as a series of one-off link writes is what creates the
need for compensating operations like
`DeleteDispatcherLinkDetails`.

### 6. The manager currently knows too much of the dance

The existing kernel-side dispatcher operations are fairly low-level:

- attach dispatcher
- attach extension
- load and pin dispatcher
- create XDP link
- create TC filter
- update XDP dispatcher link

That is fine as a low-level kernel API.

The problem is that the manager is then forced to orchestrate too
much of the rebuild dance in detail:

- load new dispatcher
- attach extensions
- update or create attach-point object
- mutate store state in the right sequence
- clean up old revision objects

The manager should own policy:

- what the desired member set is
- what ordering rules apply
- whether the dispatcher should exist at all

It should not own persistence choreography.

## Diagnosis in Ousterhout terms

The dispatcher subsystem is currently a shallow module.

Its interface is large relative to the amount of complexity it hides.
Indeed, it does not hide enough complexity; it exports internal
representation and update sequencing to its callers.

Examples:

- `DeleteDispatcherLinkDetails` leaks table structure
- `IncrementRevision` leaks update sequencing
- `CountDispatcherLinks` leaks how emptiness is determined
- `ListDispatcherSlots` leaks how membership is reconstructed
- `SaveLink` leaks a row-oriented model onto a set-replacement
  problem

A better design would make the dispatcher subsystem a deep module by
exposing only a few meaningful operations while burying the messiness
of:

- revision replacement
- row deletion ordering
- registry/detail coherence
- XDP vs TC persistence differences
- empty-dispatcher teardown rules

## Design principles

### 1. A dispatcher is a logical attach point

A dispatcher is identified by stable attach-point identity:

- dispatcher type
- network namespace ID
- interface index

For TC, direction is part of dispatcher type (`tc-ingress`,
`tc-egress`).

This logical identity remains stable across rebuilds even as kernel
program IDs and link IDs change.

The codebase already represents this as `dispatcher.Key`.

### 2. Dispatcher persistence is snapshot-based

The persistent meaning of a dispatcher is:

- current logical identity
- current revision
- current realised attach-point data
- current member set

This is a snapshot, not a bag of rows manipulated separately.

### 3. Revision is read-model metadata, not load-bearing

Revision is part of the snapshot and is visible in CLI output,
debugging, and e2e tests. It is not load-bearing in manager logic.

No manager decision should depend on the current revision number.
The manager cares about logical dispatcher identity and current
membership. Revision is a monotonically increasing counter that
records how many times the snapshot has been replaced; it is useful
for diagnostics and human inspection, not for control flow.

### 4. Dispatcher replacement is atomic

Replacing the current persisted dispatcher revision is one store
operation, not a protocol assembled by the caller.

The store must atomically:

- replace the persisted dispatcher state
- remove old extension membership rows
- insert new extension membership rows

with no caller-visible intermediate representation.

This atomicity guarantee is a store guarantee. It does not imply
that the kernel transition itself is atomic. The higher-level overall
flow remains:

1. realise a new dispatcher revision in the kernel
2. persist the new snapshot atomically
3. clean up old kernel revision objects

The point of this redesign is that step 2 becomes one semantic store
operation rather than a row choreography performed by the manager.

### 5. Row layout is internal

The store may continue to use:

- `dispatchers`
- `links`
- `link_xdp_details`
- `link_tc_details`

but callers must not depend on that fragmentation.

### 6. SaveLink remains valid for non-dispatcher links

This rework is specifically about dispatcher persistence.

It does not require redesigning tracepoint, kprobe, uprobe, fentry,
fexit, or TCX persistence. For those cases, `SaveLink` remains a
good fit.

The design change is that dispatcher extension membership is no
longer persisted via the public `SaveLink` interface.

### 7. Dispatcher membership is the canonical source for extension links

Dispatcher-backed extension links have two potential identities:

- as entries in the general `links` table (visible to `LinkStore`)
- as members of a dispatcher snapshot (visible to `DispatcherStore`)

There must be one canonical owner per link kind, not two.

After this rework, dispatcher extension membership is owned by
`DispatcherStore`. The `links` and detail rows for extensions are
created, replaced, and deleted exclusively through
`ReplaceDispatcherSnapshot` and `DeleteDispatcherSnapshot`.

`LinkStore` continues to own non-dispatcher links (tracepoint,
kprobe, uprobe, fentry, fexit, TCX).

The failure mode to avoid is a half-way design where some dispatcher
link facts live in `LinkStore`, some live in dispatcher snapshots,
and callers must merge them. That line must be hard.

## Proposed model

The codebase already has a stable logical dispatcher identity type:
`dispatcher.Key`. This redesign builds on that existing domain model
rather than introducing a parallel key type.

### New dispatcher membership type

```go
type DispatcherMember struct {
    ProgramID   kernel.ProgramID
    ProgramName string
    ProgPinPath string

    LinkID      kernel.LinkID
    LinkPinPath string

    Position  int
    Priority  int
    ProceedOn uint32
    Ifname    string
}
```

This represents one attached extension in the current dispatcher
revision.

The exact field set may evolve, but the important point is that it
is a membership record, not a stitched-together bag of columns from
multiple tables.

### New dispatcher snapshot type

```go
// DispatcherRuntime groups the current realised dispatcher
// runtime facts: the kernel program identity and any
// type-specific attachment handle.
type DispatcherRuntime struct {
    ProgramID      kernel.ProgramID
    LinkID         *kernel.LinkID // nil for TC
    FilterPriority *uint16        // TC filter priority; nil for XDP
}

type DispatcherSnapshot struct {
    Key      dispatcher.Key
    Revision uint32
    Runtime  DispatcherRuntime
    Members  []DispatcherMember
}
```

`DispatcherRuntime` separates current kernel-realisation facts
(program ID, XDP link ID, TC filter priority) from the logical
dispatcher identity and membership. These runtime fields change
every rebuild; grouping them makes it obvious they are current
realised dispatcher state, not stable logical identity.

`DispatcherRuntime` is a persistence contract type: it describes
what gets stored as part of the snapshot. It is not the same as the
optional dispatcher runtime module described later, which would own
kernel-side rebuild mechanics. The naming overlap is deliberate:
the stored runtime facts are a record of what the runtime module
produced.

`DispatcherRuntime` must stay narrow. It contains only facts that
identify or locate the current realised dispatcher at the kernel
hook point. Member ordering, proceed-on bitmasks, link pin paths,
and similar per-extension data belong in `DispatcherMember`, not
here. If a field does not answer the question "how do I find or
address the currently attached dispatcher program?", it does not
belong in `DispatcherRuntime`.

The snapshot expresses what callers actually care about:

- which logical dispatcher this is
- what current revision is active
- what runtime object identifies the dispatcher itself
- which members are attached right now

### New dispatcher summary type

Broad enumeration does not always require materialising full member
lists. For listing, inspection, and GC-style traversal, a summary
view is sufficient:

```go
type DispatcherSummary struct {
    Key         dispatcher.Key
    Revision    uint32
    Runtime     DispatcherRuntime
    MemberCount int
}
```

`DispatcherSummary` exists to support broad read-side use cases
such as CLI listing, coherency traversal, and GC scans without
forcing full member materialisation. It keeps the full snapshot
model for precise operations while avoiding unnecessary
reconstruction work during broad scans.

`MemberCount` is always a derived projection from the underlying
membership rows, never a separately maintained truth. The
implementation must compute it from the actual member set, not
store it as an independent counter.

### New dispatcher store contract

Replace the current public dispatcher-oriented store API with:

```go
type DispatcherStore interface {
    GetDispatcherSnapshot(ctx context.Context, key dispatcher.Key) (DispatcherSnapshot, error)
    ListDispatchers(ctx context.Context) ([]DispatcherSummary, error)
    ReplaceDispatcherSnapshot(ctx context.Context, snap DispatcherSnapshot) error
    DeleteDispatcherSnapshot(ctx context.Context, key dispatcher.Key) error
}
```

#### GetDispatcherSnapshot

Returns the full current persisted snapshot for one logical
dispatcher.

#### ListDispatchers

Returns logical dispatcher summaries for all current dispatchers.

The current `ListDispatchers` returns stored dispatcher rows
(`[]dispatcher.State`). The redesigned `ListDispatchers` returns
logical dispatcher summaries (`[]DispatcherSummary`). The method
name survives, but the abstraction changes: summaries are
domain-level views, not persistence-shaped row payloads.

This is for inspection, traversal, and similar broad queries where
full membership materialisation is unnecessary.

#### ReplaceDispatcherSnapshot

Atomically replaces the current persisted snapshot for the logical
dispatcher.

This operation owns:

- revision replacement
- removal of old extension rows
- insertion of new extension rows
- preservation of registry/detail coherence

The caller must not have to manipulate extension detail rows
directly.

Again, this operation makes persisted dispatcher state atomic.
It does not by itself make the kernel transition atomic.

**Hard invariant:** replacement is keyed by `dispatcher.Key`
(logical attach-point identity), never by runtime facts such as
the current program ID or link ID. Those runtime values change
between revisions. Any implementation that uses the old program ID
as the replacement key will silently break when the dispatcher is
recompiled. This is where old habits will creep back in.

#### DeleteDispatcherSnapshot

Atomically deletes the dispatcher and all persisted extension
membership for the logical dispatcher.

This operation owns deletion ordering and must leave no stranded
base rows.

## What gets removed

The following methods should be removed from the public
`DispatcherStore` contract:

- `SaveDispatcher`
- `DeleteDispatcher`
- `IncrementRevision`
- `CountDispatcherLinks`
- `ListDispatcherSlots`
- `DeleteDispatcherLinkDetails`

These methods are implementation-shaped and make the module shallow.

They may survive temporarily as private helpers inside the SQLite
implementation during migration, but they should not remain part of
the platform contract.

## Store responsibilities after the redesign

After this rework, the store is responsible for all dispatcher
persistence mechanics.

That includes:

- mapping logical dispatcher identity to storage rows
- atomically replacing revision state
- deleting old membership rows via base-link deletion, not
  detail-row-only deletion
- maintaining registry/detail coherence
- rejecting illegal partial updates
- ensuring that `DeleteDispatcherSnapshot` removes both dispatcher
  metadata and persisted extension membership

The caller should not know or care whether this is implemented
using:

- one dispatcher table plus two detail tables
- snapshot materialisation via `links`
- subquery-based base-link deletion

That is all internal.

## SQLite implementation direction

The existing schema can still support the redesign. This rework does
not require a radically different physical schema.

What changes is the public contract and the atomicity boundary.

### Replace-the-world persistence

`ReplaceDispatcherSnapshot` should execute, within one transaction:

1. Upsert the dispatcher row for the logical key
2. Delete all old extension base link rows for that logical key
3. Insert all new extension base link rows
4. Insert all new extension detail rows
5. Commit

The key point is step 2: delete from `links`, not merely from
`link_xdp_details` or `link_tc_details`.

That preserves registry/detail coherence.

The logical-key deletion should be attach-point based, not
old-dispatcher-program-ID based, because the dispatcher program ID
changes between revisions.

For XDP:

```sql
DELETE FROM links
WHERE link_id IN (
    SELECT link_id
    FROM link_xdp_details
    WHERE nsid = ? AND ifindex = ?
);
```

For TC ingress/egress:

```sql
DELETE FROM links
WHERE link_id IN (
    SELECT link_id
    FROM link_tc_details
    WHERE nsid = ? AND ifindex = ? AND direction = ?
);
```

### Dispatcher deletion

`DeleteDispatcherSnapshot` should, within one transaction:

1. Delete extension base link rows for the logical key
2. Delete the dispatcher row
3. Commit

This replaces the old split between
`DeleteDispatcherLinkDetails` then `DeleteDispatcher` and prevents
representation leaks.

## Adjacent schema fixes

These are narrow changes outside the interface surface, but they
materially improve the dispatcher rework.

### 1. Remove ON DELETE CASCADE from dispatcher-program foreign keys

For:

- `link_xdp_details.dispatcher_program_id`
- `link_tc_details.dispatcher_program_id`

Remove `ON DELETE CASCADE`, retain `ON UPDATE CASCADE` for now.

`ON DELETE CASCADE` is actively harmful because it deletes only
detail rows and can strand base `links` rows. Dispatcher teardown
must be owned by store ordering, not hidden behind partial
schema-side cascades.

`ON UPDATE CASCADE` is retained for now but is no longer
structurally important. In the old design it partly papered over a
leaky update protocol: when a dispatcher was recompiled, the cascade
silently retargeted extension rows to the new program ID. In the
snapshot-replacement model, the store deletes and reinserts the
entire extension set every rebuild, so the cascade has nothing to
do. It remains as a local convenience inside the store
implementation. Correctness comes from the store's atomic
replacement operation, not from relying on schema-side cascade
behaviour.

### 2. Represent absent dispatcher link as NULL, not sentinel 0

For `dispatchers.link_id`:

- XDP dispatchers have a BPF link
- TC dispatchers do not

That should be expressed as NULL, not 0.

```sql
-- Before
link_id INTEGER NOT NULL DEFAULT 0,
CHECK (
    (type = 'xdp' AND link_id != 0)
    OR
    (type IN ('tc-ingress', 'tc-egress') AND link_id = 0)
)

-- After
link_id INTEGER,
CHECK (
    (type = 'xdp' AND link_id IS NOT NULL)
    OR
    (type IN ('tc-ingress', 'tc-egress') AND link_id IS NULL)
)
```

### 3. Negative synthetic link IDs

Synthetic link IDs should move from the high unsigned range to
negative values.

- `kernel.LinkID` changes from `uint32` to `int64`
- Synthetic IDs count down from `-1`: `-1`, `-2`, `-3`, etc.
- The `is_synthetic` column is dropped; synthetic status is derived
  from the sign of `link_id`
- Schema version bumps from 7 to 8; no migration is required
  because the database is reconstructible state

This avoids collision with valid kernel-assigned unsigned 32-bit IDs
and makes synthetic-versus-real a robust sign-based distinction. It
is not dispatcher-specific, but it directly improves the link model
the dispatcher snapshot depends on.

### 4. Attach-point indexes

```sql
CREATE INDEX IF NOT EXISTS idx_link_xdp_by_attach_point
    ON link_xdp_details(nsid, ifindex);

CREATE INDEX IF NOT EXISTS idx_link_tc_by_attach_point
    ON link_tc_details(nsid, ifindex, direction);
```

### 5. Program delete consistency

`Delete` in `ProgramWriter` does not return `ErrRecordNotFound` when
zero rows are affected. `DeleteLink` and `DeleteDispatcher` both do.
Program delete should follow the same pattern.

## What stays the same

Most of the non-dispatcher interface shape is acceptable and should
not be churned merely because dispatchers are messy.

In particular:

- `ProgramStore` is broadly fine
- `LinkStore` is broadly fine for non-dispatcher links
- `Transactional` is fine
- The kernel-side low-level attach primitives may remain, though
  they should increasingly become implementation detail rather than
  manager choreography surface

This rework is intentionally focused.

## Optional adjacent improvement: dispatcher runtime module

This is not required to complete the store redesign, but it is the
next natural step.

Today the kernel-facing dispatcher API is still procedural:

- load dispatcher
- attach extension
- update XDP link
- create TC filter

A later improvement would add a higher-level runtime abstraction
that takes a desired member set and realises a dispatcher revision
in the kernel, returning:

- realised current snapshot data
- cleanup work for old revision objects

That would further reduce manager complexity.

However, that is a second step. This document focuses first on
fixing the persistence boundary. Resist the temptation to invent
the runtime module during the store rework. Fix the persistence
boundary, observe what manager complexity remains, and only then
decide whether a runtime module is warranted.

## Migration plan

### Phase 1: add new dispatcher domain types

Introduce:

- `DispatcherMember`
- `DispatcherRuntime`
- `DispatcherSnapshot`
- `DispatcherSummary`

without removing the old API yet.

`dispatcher.Key` already exists in the codebase.

### Phase 2: add new snapshot-based store methods

Add:

- `GetDispatcherSnapshot`
- Retain `ListDispatchers`, but change its result from
  `[]dispatcher.State` to `[]DispatcherSummary`
- `ReplaceDispatcherSnapshot`
- `DeleteDispatcherSnapshot`

Implement them in SQLite using the current schema.

Internally, the implementation may temporarily reuse existing
helpers while the migration is in progress.

### Phase 3: migrate callers

Move all dispatcher rebuild, detach, and inspection code to the new
snapshot-based API.

At this point the manager should stop doing things like:

- increment revision explicitly
- delete dispatcher link details explicitly
- reconstruct dispatcher membership from slot rows

### Phase 4: remove old dispatcher primitives

Remove from the public platform contract:

- `SaveDispatcher`
- `DeleteDispatcher`
- `IncrementRevision`
- `CountDispatcherLinks`
- `ListDispatcherSlots`
- `DeleteDispatcherLinkDetails`

They may remain as unexported SQLite helpers briefly if useful, but
they should no longer be visible outside the store package.

## Testing strategy

Testing should separate cleanly into three layers:

- store tests for snapshot persistence semantics
- manager integration tests for dispatcher lifecycle logic
- e2e tests for externally observable behaviour

The redesign should reduce churn at the e2e layer because it changes
representation, not externally visible dispatcher behaviour.

### Store-level tests

#### 1. Replace snapshot removes old persisted membership completely

**Setup:** existing dispatcher snapshot with persisted members.

**Action:** call `ReplaceDispatcherSnapshot` with a new revision and
a different member set.

**Assert:** old base `links` rows are gone, old detail rows are
gone, new base/detail rows exist, current dispatcher row reflects
the new revision and program ID.

#### 2. Delete snapshot removes everything

**Setup:** dispatcher snapshot with persisted members.

**Action:** call `DeleteDispatcherSnapshot`.

**Assert:** no dispatcher row remains, no extension base link rows
remain, no extension detail rows remain.

#### 3. Failed replacement rolls back completely

**Setup:** existing dispatcher snapshot.

**Action:** attempt replacement with one invalid member row.

**Assert:** old snapshot remains intact, no partial new rows exist,
registry/detail coherence is preserved.

#### 4. Empty dispatcher detection becomes snapshot-based

Callers should not need `CountDispatcherLinks` to decide what
current membership is. They should inspect the current snapshot's
member set directly.

That becomes the new testable behaviour.

### E2E test impact

The existing e2e dispatcher suite is mostly behavioural and should
survive this redesign with little or no semantic churn.

That is desirable. The redesign is intended to change persistence
boundaries and internal reconstruction, not the observable lifecycle
or packet-processing behaviour of dispatcher-backed attachments.

#### Behavioural tests that should remain valid

These tests assert externally visible semantics and should continue
to hold:

- priority ordering and tie-break behaviour
- zero-priority default ordering
- max-programs enforcement
- slot reuse after detach
- dispatcher teardown after last detach
- multiple-interface independence
- ingress/egress independence
- XDP and TC chain execution under real traffic
- TC proceed-on chain-break behaviour
- fill/drain/refill behaviour
- GC preserving live dispatcher-backed extensions

These tests are valuable precisely because they do not depend on
how dispatcher state is fragmented across tables.

#### E2E helpers that are currently too store-shaped

Some current helpers expose the old representation more directly
than they should.

In particular:

- `CountDispatcherExtensions`
- Tests that speak in terms of "extension count"
- Tests that call `GetDispatcher` only to derive existence and
  member count

These are not bad tests, but they are asking the old question:
"how many extension rows exist?"

The behavioural question is: "what is the current member set of
this logical dispatcher?"

The helper surface should therefore move from row-count language to
snapshot language.

Concretely, prefer:

- `GetDispatcherSnapshot(...).Members`
- Or a manager-level helper that returns current dispatcher member
  count
- Or an e2e observer helper that exposes dispatcher existence and
  member count without exposing store representation

#### Recommended e2e helper refactor

Introduce an observation helper along these lines:

```go
type DispatcherObservedState struct {
    Exists      bool
    MemberCount int
    Revision    uint32
}

func (e *TestEnv) ObserveDispatcher(
    ctx context.Context,
    key dispatcher.Key,
) (DispatcherObservedState, error)
```

This lets e2e tests ask the semantic questions they actually care
about:

- Does the dispatcher exist?
- How many members does it currently have?
- What revision is currently active?

Keep this helper intentionally thin. It must not grow into a second
`GetDispatcherSnapshot` under a softer name. Its purpose is to
expose the minimum observable facts needed for behavioural
assertions. If a test needs full membership detail, it should call
`GetDispatcherSnapshot` directly rather than inflating the observer.

This reduces future churn by ensuring the e2e suite depends on
logical dispatcher observations rather than store-shaped helpers.

#### Terminology cleanup in e2e tests

Rename "extension count" to "member count" in e2e helpers and
assertion text.

That aligns the tests with the redesigned model:

- dispatcher membership is the primary concept
- row count is an implementation detail

#### Assertions that may be relaxed

Some e2e tests assert that dispatcher-backed link details expose:

- non-zero dispatcher ID
- non-zero revision

Revision is part of the public read model (see design principle 3),
so asserting non-zero revision in e2e tests is acceptable.

However, no e2e test should depend on a specific revision value or
use revision to drive control flow. Revision is diagnostic metadata.
The stronger e2e assertions are the ones that verify:

- attachment exists or is removed
- traffic flows correctly
- ordering is correct
- teardown and GC behaviour are correct

### Integration tests

Manager-level tests should continue to verify the boundary between:

- kernel realisation
- snapshot persistence
- old-revision cleanup

In particular, they should verify that:

- A new dispatcher revision can be realised, persisted, and then
  cleanly replace the old one
- Last-member detach removes the logical dispatcher from persisted
  state
- Failure after kernel realisation but before persisted replacement
  is handled coherently
- GC reasons about current logical membership rather than stale
  row-level dispatcher-link details

The most important integration tests are the awkward failure
boundaries. These are where the design will prove itself; store
tests alone will not flush them out:

- **Kernel realised, persist fails:** a new dispatcher revision
  has been loaded and attached in the kernel, but
  `ReplaceDispatcherSnapshot` returns an error. The manager must
  handle this without leaving stale kernel objects permanently
  untracked.

- **Persist succeeds, old-revision cleanup fails:** the new
  snapshot is persisted, but cleanup of old kernel objects (old
  dispatcher program, old links) fails. The system must converge
  on the next GC pass rather than leaving permanent debris.

- **Last-member detach succeeds in kernel, persisted delete fails:**
  the kernel-side detach and dispatcher removal succeed, but
  `DeleteDispatcherSnapshot` returns an error. The persisted state
  is now stale; GC must be able to reconcile.

- **GC sees mixed old/new runtime remnants:** after a partial
  failure, kernel state may contain objects from both the old and
  new dispatcher revisions. GC must reason about current logical
  membership from the persisted snapshot and clean up anything
  that does not belong to it.

## Non-goals

This rework does not attempt to:

- redesign all platform interfaces
- replace the existing non-dispatcher `LinkStore` model
- add trigger-based enforcement for every registry/detail invariant
- redesign the kernel adapter in the same change
- unify TCX with dispatcher-based TC attachment

It is deliberately focused on making dispatcher handling deep and
coherent.

## Final design rule

A caller must never have to know how dispatcher state is fragmented
across tables.

A caller should only be able to express:

- "what is the current dispatcher snapshot?"
- "replace it with this new snapshot"
- "delete this dispatcher snapshot"

Everything else is internal machinery. Table layout is an
implementation detail; snapshot replacement is the abstraction.

That is the deep module boundary this rework is trying to establish.
