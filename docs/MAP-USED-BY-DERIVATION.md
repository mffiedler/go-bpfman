# MapUsedBy derivation: where map-set membership belongs

This note records a change of understanding about finding #5 in
`PR161-TODO.md` ("ListPrograms issues one ListMapSetUsers query per
program (N+1)"). The finding is real, but framing it as a query-count
problem understates it. The underlying issue is an altitude violation:
read-path map-set membership was attached at the manager layer instead of
being derived where the rest of the read model is derived.

**Status:** implemented in commit `520d2174`. The plan below landed as
written, with one refinement to step 5 recorded there: the load response
is also an in-process contract (the shell asserts on it), not gRPC-only,
so the field stays on `Manager.Load` and is derived inside the load
transaction. The present-tense diagnosis that follows describes the state
before that change.

## Initial understanding (as framed in the doc)

`PR161-TODO.md` #5 framed this as a performance defect:

- `ListPrograms` calls `enrichMapSetUsers` once per matching program.
- Each call issues `SELECT program_id FROM managed_programs WHERE
  map_set_id = ?` against sqlite.
- For K listed programs that is K round-trips.
- Fix: build the `map_set_id -> users` grouping once in memory rather
  than round-tripping K times.

The implied remedy was local: collect the grouping inside `ListPrograms`
(or a manager helper) and look each program up. A manager-local attempt
along these lines (a `mapSetUsersFromSnapshot` helper walking
`obs.Programs` and grouping by `mapSetID`, with `ListPrograms` doing a map
lookup) was tried and reverted, because it leaves the derivation at the
manager altitude and leaves the single-get and load paths still calling
the store. The current code is therefore the original PR state:
`enrichMapSetUsers` (`manager/list.go:312`) calls `store.ListMapSetUsers`
once per program from `ListPrograms` (`manager/list.go:406`), the single
get (`manager/list.go:192`), and the load path -- the N+1 this change
removes.

## New understanding

Two facts, established by reading the store and the snapshot, change the
diagnosis.

First, `map_set_users` is not a table. The membership queries read a
column:

```
SELECT COUNT(*)    FROM managed_programs WHERE map_set_id = ?   -- CountMapSetUsers
SELECT program_id  FROM managed_programs WHERE map_set_id = ?   -- ListMapSetUsers
```

(`platform/store/sqlite/stmts.go:90,95`). `map_set_id` is a column on the
program row, equal to `MapOwnerID` if set else the program's own id
(`mapSetID`, `manager/list.go:317`). The separate `map_sets` table holds
only set identity `(id, pin_path)`, not membership. So map-set membership
is not a second source of truth that must be joined; it is a field on the
record.

Second, `inspect.Snapshot` already pulls every record. It calls
`store.List(ctx)` (`inspect/inspect.go:405`), which returns
`map[ProgramID]ProgramRecord`, and every record carries
`Handles.MapOwnerID`. So the snapshot already holds, in memory,
everything required to compute the grouping. `ListMapSetUsers(id)` is
exactly "group the records the snapshot already has by `mapSetID`, take
bucket `id`".

Together these say the read-path `ListMapSetUsers` calls are pure
redundancy: they round-trip to sqlite to recompute a grouping over data
the caller already holds. The N+1 was the visible symptom; the cause is
that `MapUsedBy` -- derived state of exactly the kind `inspect.Snapshot`
exists to compute -- was derived in `manager` instead, bypassing the
snapshot and re-querying the store per program.

`inspect.Snapshot` is the established derived-state builder: it correlates
kernel, filesystem and store, derives presence, and sorts for
determinism, so that every read path (`ListPrograms`,
`FindLoadedProgramByMetadata`, the single-program get) shares one
coherent projection. `MapUsedBy` should be part of that projection. The
reverted `mapSetUsersFromSnapshot` attempt was the right computation at
the wrong altitude: it reached into `obs.Programs` from the manager and
reimplemented grouping that the snapshot could expose once for all
consumers, and it only covered `ListPrograms` -- the single-program get
path (`manager/list.go:192`) and the load path still called
`store.ListMapSetUsers`. In the current (reverted) state all three read
paths pay the per-program store read.

This is the same shape as the other findings on this branch: the
post-commit best-effort wart (#2) and the per-program store read (#5)
both follow from the read projection being assembled beside the snapshot
rather than inside it.

## The plan

Move the derivation to the altitude the design already uses.

1. In `inspect.Snapshot`, after `store.List`, build the
   `mapSetID -> []ProgramID` grouping once from the records, ranging over
   all store rows (so a store row whose kernel object is absent still
   counts, matching the old `ListMapSetUsers` semantics). Sort each
   bucket by program id.

2. Expose it as derived state on the observation: a `MapUsedBy
   []ProgramID` field on `ProgramView`, populated from the grouping, or a
   grouping accessor on `Observation`. This makes the snapshot the single
   place membership is computed.

3. Have every read path read it from the snapshot:
   - `ListPrograms` reads `view.MapUsedBy`; drop the per-program
     `enrichMapSetUsers` call.
   - `FindLoadedProgramByMetadata` and the single-program get
     (`manager/list.go:192`) read the same field; drop `enrichMapSetUsers`
     / `mapSetUsers` on the read path.

4. Keep the store's per-id `CountMapSetUsers` / `ListMapSetUsers` for the
   mutation and GC paths only -- the "delete the set after the last user
   unloads" refcount runs under the writer lock with an id in hand and no
   snapshot, so a per-id authoritative query is correct there. This is
   not a deletion of the store API, only of its use for read enrichment.

5. The load response still carries `MapUsedBy`, both for gRPC
   LoadResponse parity and because the in-process shell asserts on it
   (the `*_LoadAndGet` scripts match it exhaustively on the load output),
   so it stays on `Manager.Load`. It is derived inside the phase-B
   transaction from `tx.List` and the same `MapSetMembers` grouping,
   atomic with the save: the membership read commits or rolls back with
   the load rather than running as a best-effort post-commit decoration.
   This removes the post-commit fallible read that #2 had to make
   best-effort.

## Why this is better than the doc's framing

The doc's framing fixes the query count in one function and stops. It
leaves three things standing: the derivation lives in `manager` reaching
into snapshot internals; the single-get and load paths still issue the
redundant store read; and a future read path that wants `MapUsedBy` will
reach for `enrichMapSetUsers` and reintroduce the per-program round-trip,
because that is still the path of least resistance.

Deriving in the snapshot fixes the cause, not the symptom:

- One computation, one altitude. Membership is derived once where all
  other derived state is, so every consumer is consistent by
  construction and no consumer can reintroduce the N+1.
- No store reads on the read path at all -- not "one bulk query instead
  of K", but zero, because the snapshot already fetched the records and
  membership is a field on them. No bulk store accessor is needed; adding
  one would solve a problem we can design away.
- Single source of truth. Membership comes from the records'
  `MapOwnerID`, so it cannot diverge from a parallel store of the same
  fact.
- It removes, rather than papers over, the #2 post-commit read: the load
  response is filled from derived state instead of a fallible store query
  after the commit boundary.

The cost is a wider diff -- it touches `inspect`'s `Observation` /
`ProgramView` and three manager call sites rather than one function -- so
it is a candidate for a follow-up to the parity merge rather than a
late edit to it. But it is the change that puts the feature back inside
the existing design instead of beside it.
