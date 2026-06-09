# Link Identity Design

This document describes the current link identity model in this Go
implementation, why it is wrong, how the Rust implementation models the same
problem, and the proposed Go/SQLite model.

## Current Go Model

The current Go model has a single `LinkRecord.ID` field:

```go
type LinkRecord struct {
	ID        kernel.LinkID    `json:"id"`
	ProgramID kernel.ProgramID `json:"program_id"`
	Kind      LinkKind         `json:"kind"`
	PinPath   *LinkPath        `json:"pin_path"`
	Details   LinkDetails      `json:"details"`
	CreatedAt time.Time        `json:"created_at"`
}
```

That one value is used for three different concepts:

- the bpfman user-facing link handle;
- the SQLite primary key for the managed link record;
- the kernel `bpf_link` ID, when the attach operation creates one.

For attach types that do not produce a kernel `bpf_link` ID, the code
generates random synthetic IDs in the upper half of the `uint32` namespace:

```go
const SyntheticLinkIDBase = 0x80000000
```

The SQLite schema encodes this convention with `is_synthetic` and a range
check:

```sql
CHECK (
    (is_synthetic = 1 AND link_id >= 2147483648)
    OR
    (is_synthetic = 0 AND link_id < 2147483648)
)
```

The intent is practical: bpfman needs a stable handle for every managed
attachment, including perf-event based attachments that do not have a real
kernel link object.

## Why This Is Wrong

The problem is not that bpfman needs an ID. The problem is that the current
model uses a value typed as `kernel.LinkID` for something that is not always a
kernel link ID.

That causes several modelling errors:

- A synthetic ID is not a kernel ID, but it has the kernel ID type.
- The `id` field in API and CLI output sometimes means "kernel `bpf_link` ID"
  and sometimes means "bpfman management handle".
- The synthetic range assumes the kernel will not return values in the upper
  half of the `uint32` namespace. The current implementation chooses random
  IDs in that range; that is a bpfman convention, not a kernel guarantee.
- `IsSyntheticLinkID(id)` makes object type depend on an integer range rather
  than on the shape of the object.
- `HasKernelLinkID` is not truthful for `LinkRecord`, because a managed link
  record can exist without any kernel link ID.
- `AttachOutput.Synthetic` is a second representation of the same fact already
  encoded by the integer range and SQLite `is_synthetic` flag.

This is an identity-domain bug. A bpfman-managed attachment and a kernel
`bpf_link` object are related, but they are not the same entity.

## What Rust Does

The Rust implementation already treats link identity as bpfman-owned.

In Rust, `LinkData::new()` creates a random `u32` before attach and stores it
as the link's `id` in a temporary sled tree. After attach succeeds, that tree
is finalised as:

```text
link_{id}
```

Attach returns this bpfman-generated ID in `AttachResponse.link_id`.
Detach and get open the bpfman DB tree by that ID.

The kernel/Aya link token returned by `attach()` is local to the attach
operation. The resulting FD-backed link is pinned under:

```text
/run/bpfman/fs/links/{bpfman_link_id}
```

Dispatcher rebuilds follow the same identity model. Rust rebuilds the whole
dispatcher revision, freplace-attaches every current member, consumes the
fresh Aya link token only to take and pin an `FdLink` under the
revision/position path, and does not write that kernel-side link token back to
the member `Link`. The persisted member identity remains the pre-existing
`LinkData` ID.

This means Rust's `bpfman list links` `Link ID` column is not claiming to be
the same ID that `bpftool link show` reports. It is a bpfman management
identity.

That model is better than the current Go model because it avoids reserving
part of the kernel ID space. However, Rust does not make the distinction very
explicit in the API shape: the field is still just called `link_id`, and there
is no first-class nullable kernel link ID for correlation with bpftool.

## Improvement Over Rust

The Go model should keep Rust's core idea:

- bpfman link IDs are bpfman-owned management handles;
- kernel link IDs are optional observed/correlated kernel attributes.

But it should make that distinction explicit in types, storage, API output,
and CLI output.

Go should not copy Rust's in-process random allocation point. Rust can carry a
stable dispatcher member ID through rebuild because it allocates the bpfman ID
before attach. The Go design deliberately uses SQLite `AUTOINCREMENT` instead,
so handles are never reused after deletion. The cost of that choice is that
new IDs only exist after insertion; store operations that allocate IDs must
return the completed record or snapshot.

The improved model is:

- `id`: bpfman link ID, always present, accepted by `bpfman link get` and
  `bpfman link detach`;
- `kernel_link_id`: kernel `bpf_link` ID captured by bpfman for this
  attachment, if any;
- `pin_path`: optional bpffs pin for the link, independent of whether the
  kernel link ID is present;

Those columns answer different questions and should not be constrained to each
other in the schema. In today's attach paths they happen to move together:
container uprobes have neither a captured kernel link ID nor a bpffs pin, while
other currently managed links have both.

There is no synthetic concept in the new model. Do not add a renamed
replacement for it. In particular, the new implementation should have no:

- synthetic ID range;
- synthetic ID generator;
- synthetic ID predicate;
- synthetic field on attach output;
- synthetic field in the database;
- comments or tests that describe a surviving synthetic identity model.

The only question the model answers is whether bpfman captured a kernel link
ID for the managed attachment.

The UI should not imply that `bpfman list links` is the same inventory as
`bpftool link show`.

`bpftool link show` lists kernel `bpf_link` objects.
`bpfman link list` lists bpfman-managed attachments.

For some bpfman-managed attachments, bpfman has captured a kernel `bpf_link`
ID. For others, it has not.

The default table should expose this clearly:

```text
ID  KIND        PROGRAM  KERNEL-LINK  PINNED  ATTACHMENT
1   tcx         42       19           yes     eth0 ingress pos-0
2   kprobe      43       20           yes     do_sys_open
3   tracepoint  44       21           yes     sched/sched_switch
4   uprobe      45       -            no      /usr/bin/foo:malloc
```

Here `ID` is the bpfman management ID. `KERNEL-LINK` is the bpftool-visible
kernel link ID captured by bpfman, or `-` when bpfman has not captured one.

JSON should use explicit field names:

```json
{
  "id": 4,
  "program_id": 45,
  "kernel_link_id": null,
  "kind": "uprobe",
  "pin_path": null,
  "details": {
    "target": "/usr/bin/foo",
    "fn_name": "malloc",
    "offset": 0,
    "retprobe": false
  }
}
```

## Go Types

Add a bpfman-owned link ID type. Do not use `kernel.LinkID` for the managed
record key. This type is deliberately wider than `kernel.LinkID`: it is a
bpfman management handle, so it should not inherit the kernel namespace's
width or lifecycle.

```go
// LinkID uniquely identifies a bpfman-managed attachment.
type LinkID uint64
```

The backing store is SQLite `INTEGER PRIMARY KEY AUTOINCREMENT`, so the
effective range is signed 64-bit even though the Go type is `uint64`. The
important distinction is that the bpfman handle is wider than the kernel's
`uint32` ID namespace and has a store-owned lifecycle.

Keep `kernel.LinkID` for kernel `bpf_link` IDs only:

```go
package kernel

type LinkID uint32
```

Update `LinkRecord`:

```go
type LinkRecord struct {
	ID           LinkID          `json:"id"`
	ProgramID    kernel.ProgramID `json:"program_id"`
	KernelLinkID *kernel.LinkID  `json:"kernel_link_id"`
	Kind         LinkKind        `json:"kind"`
	PinPath      *LinkPath       `json:"pin_path"`
	Details      LinkDetails     `json:"details"`
	CreatedAt    time.Time       `json:"created_at"`
}
```

Use a separate input type when creating a link. A `LinkRecord` is a persisted
fact and always has a bpfman ID; a `LinkSpec` is the requested record before
SQLite has allocated that ID.

```go
type LinkSpec struct {
	ProgramID     kernel.ProgramID `json:"program_id"`
	KernelLinkID  *kernel.LinkID   `json:"kernel_link_id"`
	Kind          LinkKind         `json:"kind"`
	PinPath       *LinkPath        `json:"pin_path"`
	Details       LinkDetails      `json:"details"`
}
```

Update `LinkStatus` to keep the kernel-observed object as status:

```go
type LinkStatus struct {
	Kernel     *kernel.Link `json:"kernel"`
	KernelSeen bool         `json:"kernel_seen"`
	PinPresent bool         `json:"pin_present"`
}
```

`KernelSeen` means a non-null `kernel_link_id` was observed in the kernel link
inventory. For rows with `kernel_link_id == nil`, `KernelSeen` should be false
without implying drift.

Replace the current capability interface:

```go
type HasKernelLinkID interface {
	KernelLinkID() kernel.LinkID
}
```

with two separate capabilities:

```go
type HasLinkID interface {
	LinkID() LinkID
}

type HasKernelLinkID interface {
	CapturedKernelLinkID() (kernel.LinkID, bool)
}
```

Most CLI and shell argument parsing should use `HasLinkID`, because bpfman
commands operate on bpfman-managed attachments. The concrete code update
targets include the shell command/origin paths that currently extract link IDs
through `HasKernelLinkID`, notably
`cmd/bpfman-shell/internal/builtins/bpfman/external.go` and
`cmd/bpfman-shell/internal/builtins/bpfman/command.go`.

## Attach Output

`AttachOutput` should report only kernel state captured by the attach path.
It should not carry a bpfman link ID, a synthetic kernel-shaped ID, or any
flag that classifies the attachment as synthetic.

```go
type AttachOutput struct {
	KernelLinkID *kernel.LinkID
	KernelLink   *kernel.Link
	PinPath      LinkPath
}
```

The store should allocate the bpfman `LinkID` during record creation. The
kernel adapter should only report kernel link IDs it actually captured.

For an attach with a captured kernel link ID:

```go
AttachOutput{
	KernelLinkID: &id,
	KernelLink:   &link,
	PinPath:      pinPath,
}
```

For an attach where bpfman did not capture a kernel link ID:

```go
AttachOutput{
	KernelLinkID: nil,
	KernelLink:   nil,
	PinPath:      "",
}
```

There is no `Synthetic` boolean. Absence of `KernelLinkID` means bpfman did
not capture a kernel `bpf_link` ID for this attachment. It does not, by
itself, prove that no kernel object exists. This deletes the existing
`AttachOutput.Synthetic` field and the concept behind it; it must not be
recreated under another name.

`AttachOutput.PinPath` must be truthful. It names a bpffs link pin that the
attach path actually created. If no link was pinned, it must be empty so the
stored `LinkRecord.PinPath` becomes `NULL`. Do not return a path that merely
describes where a link would have been pinned.

For example, container uprobes currently keep the perf-event link FD in an
in-memory map because that link cannot be pinned to bpffs. Under this model
they return no captured kernel link ID and no pin path. Inspection then reports
`KernelSeen=false` and `PinPresent=false`, because bpfman has no kernel ID or
bpffs pin to correlate. That is the honest state; these attachments already do
not survive a manager restart.

## SQLite Schema

Replace the overloaded `links.link_id` schema with separate managed and kernel
identity columns.

```sql
CREATE TABLE IF NOT EXISTS links (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    kind           TEXT NOT NULL CHECK (kind IN (
                       'tracepoint','kprobe','kretprobe','uprobe','uretprobe',
                       'fentry','fexit','xdp','tc','tcx'
                   )),
    kernel_prog_id INTEGER NOT NULL,
    kernel_link_id INTEGER,
    pin_path       TEXT,
    created_at     TEXT NOT NULL,

    FOREIGN KEY (kernel_prog_id)
        REFERENCES managed_programs(program_id)
        ON DELETE CASCADE
) STRICT;
```

Add useful indexes:

```sql
CREATE INDEX IF NOT EXISTS idx_links_by_prog
ON links(kernel_prog_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_links_kernel_link_id
ON links(kernel_link_id)
WHERE kernel_link_id IS NOT NULL;
```

`kernel_link_id` is unique for non-null values. Mutating operations are
serialised and the store is authoritative: detach removes the bpfman row as
part of the same managed operation that releases the kernel link. Under that
model, two live managed attachments must not claim the same kernel `bpf_link`
ID. The partial unique index encodes that invariant while still allowing any
number of managed links with `kernel_link_id IS NULL`.

The `AUTOINCREMENT` on `links.id` is deliberate. Bpfman link IDs are
user-facing management handles; once a handle has referred to one attachment,
it should not be silently reused for a different attachment after deletion.

All detail tables should reference `links(id)`, not `links(link_id)`:

```sql
CREATE TABLE IF NOT EXISTS link_kprobe_details (
    id INTEGER PRIMARY KEY,
    fn_name TEXT NOT NULL,
    offset INTEGER NOT NULL DEFAULT 0 CHECK (offset >= 0),
    retprobe INTEGER NOT NULL DEFAULT 0 CHECK (retprobe IN (0, 1)),

    FOREIGN KEY (id)
        REFERENCES links(id)
        ON DELETE CASCADE
) STRICT;
```

The same rename applies to every `link_*_details` table.

## Store API

Store methods should use bpfman link IDs for managed link operations:

```go
GetLink(ctx context.Context, id bpfman.LinkID) (bpfman.LinkRecord, error)
DeleteLink(ctx context.Context, id bpfman.LinkID) error
CreateLink(ctx context.Context, spec bpfman.LinkSpec) (bpfman.LinkRecord, error)
ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error)
ListLinksByProgram(ctx context.Context, programID kernel.ProgramID) ([]bpfman.LinkRecord, error)
```

`CreateLink` should allocate the managed ID for new standalone links and
return the completed record. The action/executor layer should therefore treat
link creation as a producing action rather than a fire-and-forget store
mutation.

`CreateLink` is responsible for setting store-owned fields such as `id` and
`created_at`.

## Pin Paths

Rust pins links under:

```text
/run/bpfman/fs/links/{bpfman_link_id}
```

The current Go implementation does not use link-ID-derived pin paths. It pins
ordinary links by program ID plus a link name:

```text
/run/bpfman/fs/links/{program_id}/{link_name}
```

That layout can stay. The link identity fix does not require a pin-path
redesign, a preallocated link ID, or a temporary-path rename step. Pin paths
must simply remain independent of kernel link IDs.

Pin paths must also be honest. `pin_path IS NOT NULL` means bpfman believes a
real bpffs link pin exists at that path. `PinPresent` should be computed by
checking that path in bpffs. There is no separate classification flag to
suppress phantom pins.

## Dispatcher Members

Dispatcher-backed XDP and TC member links need explicit treatment because they
are not detached through the generic `DeleteLink` path today. The store rejects
generic deletion for `xdp` and `tc` link rows, and dispatcher snapshot
replacement owns their lifecycle.

In the current model, every `xdp` and `tc` link row is a dispatcher member.
`tcx` is not dispatcher-backed: it is kernel-native, carries a captured kernel
link ID when attached successfully, and is detached through the generic managed
link path.

XDP and TC hooks are single-program attachment points, so bpfman attaches one
dispatcher program to the interface hook and freplace-attaches managed user
programs into dispatcher slots. The dispatcher then calls those slots in
priority order and applies the configured proceed-on rules.

The slot set is baked into each dispatcher program revision. Any membership or
ordering change therefore rebuilds the whole dispatcher: load a new dispatcher
program, freplace every current member into the new revision, swap the hook to
the new dispatcher, and tear down the old revision. This means adding or
removing one member recreates the kernel extension links for every surviving
member, not only for the changed member.

Each dispatcher member should still get a bpfman `LinkID`. That ID identifies
the managed extension attachment and is the value accepted by `bpfman link
detach`.

This is not only a modelling clean-up; the previous rebuild path already
exposed the bug. In both XDP and TC dispatcher rebuilds, every extension was
reattached and produced a fresh kernel link ID, but existing members then
preserved the old stored ID. That made `DispatcherMember.LinkID` act like a
stable bpfman handle while it was still typed and stored as a kernel link ID.
After a rebuild, it could be a stale kernel link ID that no longer named the
live extension link.

The bug is latent rather than fatal because teardown does not use this stale ID
as the kernel handle. Detaching a dispatcher member rebuilds the snapshot
without that member; old extension links die with the old revision's
revision/position-derived pin directory. Generic `DeleteLink` rejects
dispatcher-backed members. The stale ID therefore leaks primarily into
inspection, correlation, and user-visible listings: after a sibling membership
change, bpfman may display a member link ID that `bpftool link show` no longer
contains.

`kernel_link_id` for a dispatcher member should describe the member's own
current kernel link ID captured by bpfman, if any. For current dispatcher
extension rows:

- freplace-attached dispatcher members should store the fresh kernel link ID in
  `kernel_link_id` on every rebuild;
- for dispatcher members, `kernel_link_id = NULL` should be treated as an
  anomaly unless a future member mechanism genuinely cannot capture a kernel
  link ID;
- do not derive either case from an integer range.

Persisted dispatcher snapshots should therefore carry both values:

```go
type DispatcherMember struct {
	LinkID       bpfman.LinkID
	KernelLinkID *kernel.LinkID
	ProgramID    kernel.ProgramID
	// existing priority, position, pin, and proceed_on fields...
}
```

Snapshot inputs should use a separate member spec. Unlike standalone
`LinkSpec`, this input can optionally identify an existing member, because a
whole-dispatcher replacement naturally contains both existing members whose
bpfman handles must be preserved and new members that need store allocation.

```go
type DispatcherMemberSpec struct {
	ExistingLinkID *bpfman.LinkID // nil means allocate a new bpfman LinkID
	KernelLinkID   *kernel.LinkID // refreshed from the latest extension attach
	ProgramID      kernel.ProgramID
	// existing priority, position, pin, and proceed_on fields...
}
```

On rebuild:

- existing members preserve `LinkID`;
- specs with `ExistingLinkID != nil` preserve that `LinkID`;
- specs with `ExistingLinkID == nil` receive a new `LinkID` from SQLite inside
  `ReplaceDispatcherSnapshot`;
- every member's `KernelLinkID` is refreshed from the latest extension attach
  output.

Snapshot replacement should insert `links.id = *spec.ExistingLinkID` when an
existing handle is present, let SQLite allocate `links.id` for new members, and
write `links.kernel_link_id = spec.KernelLinkID` for all members. Allocation
should remain SQLite-owned and transaction-local: dispatcher extension pin
paths are revision/position-derived, not link-ID-derived, so no external
allocator or ID reservation step is needed.

For new member rows, snapshot replacement must also populate store-owned
columns such as `created_at`.

`ReplaceDispatcherSnapshot` must be a producing operation, just like
`CreateLink`. It should allocate IDs for new members and return the completed
snapshot with every member's `LinkID` populated. The executor must use that
completed snapshot as the authoritative post-rebuild state. Otherwise a new
member would not acquire a stable bpfman handle for subsequent rebuilds.

The current range-based classification branch in dispatcher storage should
become a nil-check on `member.KernelLinkID`.

Detach routing already has the right shape: `xdp` and `tc` records route
through dispatcher rebuild, while other link kinds use generic `DeleteLink`.
The implementation should thread `bpfman.LinkID` through that path, including
`Manager.Detach`, `detachPlan`, `action.DeleteLink.LinkID`,
`action.RebuildDispatcherForDetach.ExcludeLinkID`,
`rebuildDispatcherForDetach`, and the member filter that excludes the detached
member. The filter should compare stable bpfman member handles, not kernel
link IDs.

Dispatcher runtime rows are separate from member rows. The dispatcher program's
own link ID, where present, should be renamed to `KernelLinkID`; it is already
typed as `*kernel.LinkID` and represents the dispatcher program's own kernel
`bpf_link`, not a bpfman member handle. This runtime field is nil for TC
dispatchers because TC dispatchers attach through netlink/clsact filters rather
than a dispatcher `bpf_link`; that TC/XDP asymmetry applies to the dispatcher
program's own hook attachment, not to freplace member links. If bpfman needs a
managed record for the dispatcher program's own attachment, that should be a
separate managed link row; otherwise it remains dispatcher runtime correlation
data.

## CLI And API

`bpfman link attach` should return the bpfman link ID:

```json
{
  "record": {
    "id": 7,
    "program_id": 42,
    "kernel_link_id": 19,
    "kind": "tcx"
  }
}
```

`bpfman link detach 7` detaches the bpfman-managed attachment with ID `7`.

`bpfman link get 7` looks up bpfman link ID `7`.

The table output should include kernel link ID separately. In compact mode:

```text
ID  PROGRAM  KIND    KERNEL-LINK  PINNED  ATTACHMENT
7   42       tcx     19           yes     eth0 ingress pos-0
8   43       kprobe  20           yes     do_sys_open
9   45       uprobe  -            no      /usr/bin/foo:malloc
```

In JSON, `kernel_link_id` should be `null` when absent. Do not omit it; stable
shape matters for scripts.

If filtering by kernel link ID is needed, make it explicit:

```text
bpfman link list --kernel-link-id 19
```

Do not make `bpfman link get 19` guess whether `19` is a bpfman ID or a kernel
ID. Ambiguous ID lookup is how the model becomes unclear again.

## Inspection And Correlation

There is no background link GC in the normal model. Mutating operations are
serialised and update kernel state, pins, and store rows together. The store is
the source of managed-link identity.

Inspection paths still correlate store rows with kernel and filesystem
observation for `get`, `list`, diagnostics, and cleanup tooling. Those paths
should classify links by presence of `KernelLinkID`, not by synthetic range:

- if `kernel_link_id IS NULL`, bpfman has not captured a kernel `bpf_link` ID
  for this managed attachment;
- if `kernel_link_id IS NOT NULL`, that value is the captured kernel link ID to
  use for targeted lookup or snapshot correlation;
- pin presence is independent and should continue to be checked from
  `pin_path`.

This turns classification based on the removed synthetic predicate into:

```go
if link.KernelLinkID == nil {
	continue
}
```

That is the actual domain fact.

## Schema Break

There is no compatibility migration. This code has not shipped, so the correct
change is to replace the development schema and fixtures in place:

- create `links.id` as the SQLite-owned bpfman management handle;
- create `links.kernel_link_id` as the optional captured kernel link ID;
- remove `links.link_id`, `is_synthetic`, range checks, and all helper code
  that classifies links by invented ID ranges;
- remove every code path, comment, fixture, and test that treats synthetic
  link identity as a supported concept;
- update every detail table foreign key to reference `links(id)`;
- regenerate tests and fixtures around the new schema.

Existing development databases should be discarded or recreated. Do not
preserve old development link IDs.

## Testing Strategy

The type split should make the common illegal states fail at compile time:

- manager, CLI, and store APIs should accept `bpfman.LinkID` for managed link
  handles;
- kernel adapters should return `*kernel.LinkID` only for captured kernel
  `bpf_link` IDs;
- `LinkSpec` should represent a requested link before persistence and have no
  bpfman ID;
- `LinkRecord` should represent a persisted link and always have a bpfman ID;
- dispatcher member specs should represent replacement input with an optional
  `ExistingLinkID`, while persisted dispatcher members should always have a
  bpfman ID.

That type wall is deliberately useful, but it does not protect the SQLite
mapping by itself. At the SQL boundary both bpfman IDs and kernel link IDs are
stored as integers. Store tests must therefore prove that values are written to
and read from the correct columns.

Do not try to encode the current `kernel_link_id`/`pin_path` pairing as a
closed type. Those fields answer different questions and must stay
independent nullable fields. Today's attach paths happen to produce only
`(captured kernel ID, pin path)` or `(no captured kernel ID, no pin path)`, but
that is not a schema invariant.

The zero value of `LinkRecord` is still possible in Go. The public creation
API avoids an `ID == 0` sentinel by using `LinkSpec`, but tests should assert
that no persisted `LinkRecord` returned by the store has `ID == 0`.

The first implementation pass should be test-driven against the store and fake
kernel before live kernel tests. The recommended order is:

1. Producing dispatcher snapshot: a new dispatcher member receives a bpfman
   `LinkID` from `ReplaceDispatcherSnapshot`, and the completed snapshot
   returns that ID.
2. Container uprobe pin honesty: a fake attach result with
   `KernelLinkID=nil` and `PinPath=""` persists as `kernel_link_id = NULL`,
   `pin_path = NULL`, `KernelSeen=false`, and `PinPresent=false`.
3. Partial unique kernel ID: inserting two live records with the same non-null
   `kernel_link_id` fails.
4. AUTOINCREMENT never reuse: create, delete, create yields a new bpfman ID,
   not the deleted one.
5. Detach routing by kind: `xdp` and `tc` detach through dispatcher rebuild
   excluding the bpfman `LinkID`, while non-dispatcher links use generic
   `DeleteLink`.
6. Standalone pinned attach: a fake attach result with a captured kernel link
   ID and real pin path persists with a distinct bpfman ID, that
   `kernel_link_id`, a non-null `pin_path`, and `PinPresent=true`.
7. Dispatcher rebuild preserve/refresh: an existing member preserves its
   bpfman `LinkID` while refreshing `KernelLinkID` from the latest freplace
   attach result.

The fake kernel validates model wiring: spec/record conversion, store mapping,
producing operations, pin-path truth, and detach routing. It cannot prove the
kernel-facing claims for each attach type. End-to-end tests should still cover
real pin presence, bpftool correlation, container-uprobe behaviour, and
dispatcher rebuilds against the kernel.

Do not add a permanent grep-style test for the word "synthetic". Deleted
symbols and compile failures should catch real code paths; SQL, fixture, and
comment residue should be handled with a one-off review sweep.

## E2E And Integration Scripts

After the unit and store tests are green, the `e2e/scripts/*.bpfman` scripts
should be the primary integration vehicle for this redesign. They drive the
real `bpfman` CLI against real kernel state, assert on JSON output, and already
exercise the user-facing operations affected by the identity split:

- `bpfman link attach`;
- `bpfman link get`;
- `bpfman link list`;
- `bpfman link detach`;
- `bpfman dispatcher get`;
- shell `jq` assertions over structured command output.

The existing scripts are mostly forward-compatible with the new model because
they already treat `record.id` as an opaque bpfman handle for get, list, and
detach. That is the correct behaviour. Kernel-facing assertions must use
`record.kernel_link_id`, never `record.id`.

The integration coverage should add these identity-specific checks.

1. **Standalone kernel correlation.** Attach a normal pinned link, read
   `record.kernel_link_id`, assert it is not null, then run `bpftool -j link
   show id <kernel_link_id>`. The script should assert that bpftool sees the
   link and that its program ID matches the managed program.

2. **Handle and kernel ID are distinct.** For the same normal pinned link,
   assert `record.id != record.kernel_link_id`. This proves the CLI output is
   exposing the bpfman handle and captured kernel ID as separate concepts.

3. **Dispatcher member identity across rebuild.** Attach dispatcher member A,
   record A's dispatcher `link_id` and `kernel_link_id`, attach member B to
   force a rebuild, then inspect the dispatcher again. A's `link_id` must be
   unchanged, while A's `kernel_link_id` must be refreshed to the new freplace
   link. The refreshed kernel ID should also be visible through `bpftool link
   show`.

4. **Detach uses the bpfman handle.** Detach should continue to pass the
   bpfman `record.id` or dispatcher member `link_id`. Scripts should not use
   `kernel_link_id` as the detach argument.

5. **Null captured-kernel-ID case.** The real `kernel_link_id == null` path is
   the in-memory-fd container uprobe path. It should eventually have an e2e
   script asserting `kernel_link_id == null` and `pin_path == null`, but that
   fixture is heavier than the ordinary host attach scripts. Until the
   container fixture exists, keep this behaviour covered by fake-kernel and
   store tests and track it as the known integration gap.

The shell language already supports the assertions this needs. JSON null,
absent fields, and non-null values are distinguishable, so scripts can assert
strictly on `kernel_link_id == null` or `not null` without collapsing the two
states. The scripts already use `bpftool` for map inspection, so link
correlation can use the same external-command mechanism.

The highest-value first script is a standalone pinned-link correlation test:
it proves that the new `kernel_link_id` is the same kernel-visible number that
`bpftool` reports. Without that check, the suite proves the model is internally
consistent but does not prove that it matches the kernel.

## Summary

The desired model is simple:

- bpfman link ID identifies a managed attachment;
- kernel link ID identifies a kernel `bpf_link` object when bpfman has captured
  that ID;
- a managed attachment may or may not have a captured kernel link ID;
- absence of a kernel link ID is represented with `NULL`/`nil`, not with a
  made-up integer;
- there is no synthetic link ID concept in the new design;
- CLI and JSON output expose both concepts clearly.

This matches the spirit of the Rust implementation while making the identity
boundary explicit enough that users can reason about `bpfman` and `bpftool`
output side by side.
