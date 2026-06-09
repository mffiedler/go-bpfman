# Managed Object Identity

This note captures the open design question around bpfman-managed
identity for programs, links, and maps. It is deliberately not an
implementation plan for the current link-identity change. The timing
and scope are TBD.

## Current State

Today, bpfman does not use one uniform identity model for all managed
objects.

Programs are identified by the kernel program ID:

- store APIs take `kernel.ProgramID`;
- `managed_programs.program_id` is the primary identity;
- CLI program arguments parse as program IDs;
- the bpfman program ID correlates directly with `bpftool prog`.

Maps are more nuanced. Where bpfman exposes map IDs as kernel facts,
those IDs are kernel map IDs. But maps are not currently modelled as
independent managed records in the same way programs and links are.
In the Rust implementation, persisted map ownership/share state is
keyed by the owning program ID, or by `map_owner_id` when maps are
shared. The actual kernel map IDs are stored and displayed separately
as observed kernel attributes.

This is a deliberate, known asymmetry under review. It is not an
accidental miss in the link-identity change. Program IDs currently
correlate with bpftool because the bpfman program identity is the
kernel program ID. Exposed map IDs also correlate with bpftool because
they are kernel map IDs, but map ownership metadata is not an
independent managed map identity. Link IDs do not promise bpftool
correlation because they are bpfman-managed handles.

Links are moving to a different model:

- `LinkID` is a bpfman-owned management handle;
- the handle is allocated by the store;
- the handle is used for `link get`, `link detach`, and `link delete`;
- `kernel_link_id` is a separate observed kernel attribute;
- `kernel_link_id` may be absent;
- dispatcher members preserve their bpfman handle across rebuilds
  while refreshing their captured kernel link ID.

This fixes a real bug in the old link model: one value was acting as
both a management identity and a kernel `bpf_link` ID. For dispatcher
members that value could become a stale kernel ID after rebuild, while
still being preserved as the user-visible identity.

## The Broader Question

The broader question is whether bpfman should treat all managed object
identities as bpfman-owned handles, with kernel IDs represented only
as observed runtime attributes.

In that model:

- a program handle identifies the bpfman-managed program record;
- `kernel_program_id` identifies the live kernel program, if present;
- a link handle identifies the bpfman-managed attachment record;
- `kernel_link_id` identifies the live kernel link, if captured;
- a map handle identifies the bpfman-managed map record, if maps are
  modelled as independently managed objects;
- `kernel_map_id` identifies the live kernel map, if present.

The current link redesign is an instance of this model, but only for
links.

## Why Handles Are Attractive

Handles separate domain identity from implementation detail. bpfman
already has a store, manager APIs, a daemon, cleanup logic, and
operator-shaped use cases. In that architecture, the kernel is an
adapter rather than the owner of the domain model.

If bpfman is a stateful manager, kernel IDs are runtime facts. They
are useful and should be exposed, but they should not necessarily be
the primary identity of bpfman records.

This becomes important when identity must outlive, precede, or survive
changes to a kernel object:

- desired-state or declarative objects that exist before load;
- references from an operator or API object to a managed program that
  should not churn when the kernel reloads it;
- reloads where the kernel program ID changes but the managed program
  should remain the same object;
- restart-stable references that are meaningful even when kernel
  state is temporarily absent;
- management operations that should target bpfman ownership rather
  than whatever kernel ID currently exists.

A bpfman handle also makes user intent clearer. Passing a handle back
to bpfman says "operate on the object bpfman manages", not "operate on
kernel object number N".

## Why Kernel IDs Are Still Valuable

For programs, the current identity really is the kernel ID. For maps,
the exposed map IDs are kernel IDs, while the persisted ownership
model is attached to programs. Neither case is the same bug links had.

A loaded program always has a kernel program ID, and that ID is stable
for the lifetime of the kernel program object. Users can correlate
bpfman's program ID directly with `bpftool prog`. That correlation is
useful and true.

Renaming program IDs to handles without a concrete need would have a
cost:

- it hides a useful correlation with bpftool;
- it suggests bpfman minted the identity when it did not;
- it creates another identity namespace to allocate, store, expose,
  document, and migrate;
- it may diverge from Rust bpfman's current model without solving a
  current correctness bug;
- it risks abstraction for symmetry rather than for a real use case.

Rust parity is useful context, but not a hard constraint. The link
identity redesign already departs from Rust deliberately: Rust
persists only the bpfman link identity and discards freplace kernel
link IDs after pinning, while Go keeps an explicit captured
`kernel_link_id` for inspection and bpftool correlation. Diverging
from Rust is acceptable when it produces a clearer model or fixes a
real bug; it should not be used as a reason to add a speculative
identity layer without a concrete need.

Rust's program model is also useful context. On successful load, Rust
renames the temporary Sled program tree to `program_<kernel program
id>` and stores that same ID on the program record. `get`, `list`, and
`remove` operations all use the kernel program ID. Rust therefore
treats program identity as one-to-one with the kernel program ID.

Rust maps do not follow a one-to-one managed-map-record model. Map
state is stored under `map_<owner program id>`, and shared maps refer
to that owner through `map_owner_id`. Kernel map IDs are captured from
the loaded program and exposed as kernel information, not used as the
Sled identity for a standalone managed map.

The link change is different because link identity has already shown
that kernel ID and management identity are not the same thing:

- some attach mechanisms do not yield a captured kernel link ID;
- dispatcher member kernel link IDs change on rebuild;
- the user-visible identity should remain stable across those
  rebuilds.

Programs have not yet demonstrated the same need. Maps have not yet
been modelled as independent managed objects that would require their
own bpfman-owned identity.

## CLI Safety And Mental Model

The asymmetry has a user-safety cost even if programs never need
desired-state identity.

For programs, `bpfman program get 123` and `bpftool prog show id 123`
refer to the same kernel program. Cross-tool copy and paste is safe
because the bpfman program ID is the kernel program ID.

For links, `bpfman link get 123` names a bpfman-managed link handle.
`bpftool link show id 123` names a kernel `bpf_link` ID. Those two
integer spaces can overlap by chance. If a user copies a kernel link
ID from bpftool and passes it to `bpfman link detach`, bpfman may find
a live managed link handle with the same number and operate on the
wrong attachment. That is a mis-targeting risk, not just a cosmetic
inconsistency.

The ordinary bpfman workflow remains safe: users can copy `ID` from
`bpfman link list` and pass it back to `bpfman link get` or
`bpfman link detach`. The risk is specifically cross-tool
copy/paste, where a kernel ID is mistaken for a bpfman handle.

This is an independent argument for a uniform handle model. It is not
about desired state or reload stability; it is about giving users one
rule:

- `ID` is the bpfman handle passed back to bpfman;
- `KERNEL-*` fields are the kernel IDs used with bpftool.

That rule would be clearest if programs, links, and independently
managed maps all used bpfman-owned handles. However, links are the
only object type with a current correctness bug that forces the split.
For now, the cheaper mitigation is to make the asymmetry visible:

- list both `ID` and `KERNEL-PROG` for programs, even when they are
  currently the same value;
- list both `ID` and `KERNEL-LINK` for links;
- describe link command arguments as bpfman link handles, not kernel
  link IDs;
- avoid prose that says "link ID" when the precise meaning is
  "kernel link ID" or "bpfman link handle".

If users trip over cross-tool IDs in practice, that experience alone
may justify a uniform handle model, even without a desired-state or
reload-stability roadmap.

## Thin Wrapper Versus Stateful Manager

The decision depends on what bpfman is meant to be.

If bpfman is a thin controller for live kernel objects, then kernel
IDs are honest identities. In that world, program 123 in bpfman being
program 123 in bpftool is a feature.

If bpfman is a stateful manager, then kernel IDs should be observed
attributes. In that world, the managed object is a bpfman record, and
the kernel object is the current runtime incarnation of that record.

bpfman's existing store and manager structure point toward the
stateful-manager model, but the concrete roadmap should decide the
question. Desired state, reload-stable references, and pre-load
identity would all justify a uniform handle model. Without those,
program/map handles are speculative. The CLI safety issue above is a
separate axis: even if the object model does not require handles,
user comprehension and mis-targeting risk may still justify them.

The litmus test is:

**Must a managed object ever be addressable when it has no live kernel
ID?**

If yes, kernel ID is the wrong primary identity for that object type.
The object needs a bpfman-owned handle, and the kernel ID should be a
nullable observed attribute.

If no, and the object always has exactly one live kernel ID for its
entire managed lifetime, then the kernel ID is an honest identity. In
that case, a separate handle may be premature.

This single question covers the roadmap cases:

- desired-state records that exist before load;
- reload-stable references across kernel ID churn;
- objects that survive daemon restart while kernel state is
  temporarily absent;
- operator or API references that need to name bpfman ownership rather
  than a current kernel object.

## Possible Direction

Short term:

- keep the current link redesign scoped to links;
- call link `record.id` a bpfman-managed link handle in prose and
  user-facing docs;
- expose `kernel_link_id` separately for bpftool correlation;
- keep program IDs as kernel program IDs;
- be explicit in docs that program IDs currently correlate with
  bpftool, while link handles do not have to;
- describe that asymmetry as intentional and under review, with this
  document as the canonical reference.

Medium term:

- write a dedicated design for managed identity across programs,
  links, and maps;
- decide whether bpfman should expose handles uniformly;
- decide whether the cross-tool copy/paste risk is enough, on its
  own, to justify uniform handles;
- decide whether API/CLI fields should remain named `id` while docs
  call them handles, or whether future external APIs should use
  `handle` explicitly;
- decide whether `kernel_program_id`, `kernel_map_id`, and
  `kernel_link_id` should be the only fields that visually resemble
  kernel IDs.

Long term, if the manager model wins:

- introduce bpfman-owned program handles;
- store kernel program IDs as nullable runtime attributes;
- introduce map handles only if maps become independently managed
  records rather than runtime facts attached to programs;
- preserve direct bpftool correlation by displaying kernel IDs
  prominently, not by making them the management identity.

## Open Questions

- Must any managed object ever be addressable when it has no live
  kernel ID?
- Does bpfman need desired-state objects that exist before kernel load?
- Should a managed program preserve identity across reload if the
  kernel program ID changes?
- Do Kubernetes/operator references need a stable bpfman identity that
  outlives kernel state?
- Are maps first-class managed objects, ownership records keyed by
  programs, or only runtime facts attached to programs?
- Is cross-tool mis-targeting risk enough to justify program handles
  even if programs otherwise have stable kernel IDs?
- Should future user-facing APIs use the word `handle`, while legacy
  JSON keeps `id`?
- Is the bpftool correlation for programs/maps important enough to
  keep as primary identity until a concrete manager use case appears?

## Current Recommendation

Do not broaden the current link-identity PR.

Links have a concrete correctness problem, so they should use bpfman
handles now. Programs should stay kernel-ID based until a specific
capability requires bpfman-owned identity. Maps should not receive a
separate handle unless bpfman first chooses to model maps as
independently managed records.

Deferring the wider decision is cheap. The link work establishes the
pattern: store-allocated handles, nullable kernel IDs as observed
attributes, and inspection that correlates with the kernel rather than
using the kernel as identity. If a future roadmap item requires
program or map handles, extending the same pattern is an application
of an existing shape, not a new invention.

At the same time, avoid writing new prose that implies all bpfman
`id` fields are kernel IDs. The accurate current model is asymmetric:

- program IDs are kernel program IDs;
- map IDs are kernel map IDs where exposed;
- map ownership/share state may be keyed by owning program identity
  rather than by an independent managed map identity;
- link IDs are bpfman-managed handles;
- kernel link IDs are explicitly named `kernel_link_id`.

Also avoid leaving the asymmetry implicit in the CLI. Prefer tables
and help text that teach the safe rule: bpfman commands take bpfman
handles, and `KERNEL-*` fields are the values to compare with bpftool.

If bpfman moves further toward desired-state management, revisit this
document and consider applying the handle model uniformly.
