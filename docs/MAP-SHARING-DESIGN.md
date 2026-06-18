# Map sharing: managed map-set semantics and Rust parity

Status: accepted; implementing on branch `map-sharing-parity` (cut from
`origin/main`).

There are no backwards-compatibility constraints. The store schema may
change freely; no migration of existing databases is required.
Do not add compatibility shims for old store layouts or old
map-ownership behaviour. Old databases are deleted during this work.
If implementation exposes a design choice not settled here, stop and ask
before coding through it.

## Goal

We make the public map-sharing behaviour coherent and compatible with
upstream Rust where Rust expresses a real managed-program contract. Rust
is evidence, not the specification. Where Rust's implementation only
updates bookkeeping or otherwise behaves unsafely, Go must not copy that
internal behaviour as gospel.

Two defects are fixed in full, now:

1. The gRPC server fabricates a map-ownership relationship for every
   multi-program load, even when nothing is shared.
2. Explicit sharing forbids unloading the map owner before its
   dependents, where Rust allows it and keeps the shared maps alive
   until the last user is gone.

Every change is driven red-green against both the Go and the Rust
`bpfman` binaries: we first observe Rust's behaviour with the binary,
decide whether the observed behaviour is the correct managed-program
contract or a Rust implementation wart, write a test that asserts the
chosen contract, watch it fail against the current Go behaviour where
appropriate, then make it pass. We use fakeKernel tests where they fit
and `.bpfman` scripts as end-to-end proof against a real kernel.
Existing tests and scripts that assert the old behaviour are corrected,
not worked around.

Axiom: prove behaviour through observable store and runtime state, not
API responses alone. A response can be well-formed while the persisted
record or the bpffs tree is wrong, so every proof asserts the durable
state -- `get`/`list` output, the sqlite record, the `fs/` tree -- as
well as, or instead of, the response. This is deliberate: the operator
integration suite is thin on inspecting actual state, so the in-tree
gRPC and `.bpfman` proofs are where we verify what actually landed. The
`Get`-per-program ownership check, the `fs/` layout regression contract,
and the runtime-tree before/after-cleanup discipline are all instances
of this axiom.

## Branch-local A/B runtime setup

For this branch only, Go's default runtime root is temporarily changed
from `/run/bpfman` to `/run/go-bpfman` (`fs.DefaultRoot`). Rust remains
on `/run/bpfman`. This is diagnostic infrastructure, not product
behaviour, and the temporary commit must be reverted before merge.

Why: Go and Rust must run side by side while we develop parity. Stable
runtime roots let us compare the managed bpffs state directly:

    sudo diff -r /run/go-bpfman/fs /run/bpfman/fs

The split already found real contamination: `/run/bpfman` contained a
stale Go sqlite `store.db` beside Rust's sled state. That would have made
A/B evidence ambiguous. With the split, Go writes under
`/run/go-bpfman`, Rust writes under `/run/bpfman`, and cross-contamination
is visible instead of silent.

Verified after the split: a flagless `bin/bpfman program load file`
wrote its program and map pins under `/run/go-bpfman/fs/...`, a flagless
Rust load wrote under `/run/bpfman/fs/...`, neither touched the other's
tree, both programs unloaded cleanly, and both `program list` results
returned empty -- confirming the two binaries are isolated and the trees
are diffable from a zero state.

Before every proof, start from a clean state:

- no leftover bpfman kernel programs;
- no stale bpffs mounts from prior e2e runs;
- empty Go program list under `/run/go-bpfman`;
- empty Rust program list under `/run/bpfman`;
- no pinned maps or links except persistent non-program artefacts that
  are intentionally preserved (for example Rust's TUF trusted root).

Each proof should record the commands, program ids, relevant
`get`/`list` output, map pin paths, and the relevant runtime-tree
differences. Cleanup is part of the proof: unload what was loaded and
verify the kernel and both runtime trees return to the expected state.

## Runtime layout regression contract

The whole runtime root is not a byte-for-byte parity surface: Go uses
sqlite and Rust uses sled, so `db/` and other implementation-private
state may legitimately differ. The managed bpffs subtree is the parity
surface. Use whole-root diffs only when investigating contamination; use
`fs/` diffs as the regression contract.

The target is not merely "same semantic pins somewhere under `fs/`".
bpfman's runtime layout is part of the manager's observable state, so
matching Rust's `fs/` layout is a parity requirement, not a cosmetic
nicety. Where Rust consistently pre-creates empty infrastructure
directories on init, Go must pre-create the same directories. This makes
the empty baseline deterministic, makes
`diff -r /run/go-bpfman/fs /run/bpfman/fs` directly useful, and removes
the need for every test and reviewer to carry a mental ignore-list for
Rust's infrastructure. If Go creates infrastructure Rust does not, treat
that as a layout-parity bug unless investigation proves it is unavoidable
kernel state outside bpfman's control.

For equivalent operations, Go and Rust must create the same layout shape
and pin classes:

- program pins under `fs/prog_<id>`;
- explicit map-set pins under `fs/maps/<set_id>/<map_name>`;
- `LIBBPF_PIN_BY_NAME` pins under `fs/shared/<map_name>`;
- link and dispatcher pins: path contract pending an A/B proof. Before
  implementing layout parity for these, drive representative XDP, TC, and
  TCX attach cases through both binaries, record the exact `fs/` paths
  each creates, and replace this placeholder with the concrete rules. Do
  not freeze wording from memory; if Rust and Go use intentionally
  different attachment models (legacy TC dispatcher versus TCX link
  paths), stop and ask before encoding parity.
- no fabricated shared map directory for multi-program loads without an
  explicit `map_owner_id`.

Every proof records the relevant `fs/` tree before load, after load,
after owner-first unload where applicable, and after final cleanup. After
cleanup, the managed bpffs state must return to Rust's empty-layout
shape -- not a Go-specific approximation -- except for proven
kernel-created artefacts that appear equivalently on both sides (for
example `maps.debug` and `progs.debug`).

Known layout facts, established by comparing both trees at rest and
after a single load:

- Rust pre-creates empty `fs/links`, `fs/xdp`, `fs/tc-ingress`,
  `fs/tc-egress`, and `fs/dispatcher-test`; Go does not. Go must
  pre-create these to match Rust's initialised layout, so that a single
  load is the only `fs/` difference under test.
- `fs/maps.debug` and `fs/progs.debug` appear in both Go and Rust. They
  are kernel-provided bpffs files listing global BPF maps and programs,
  present in any bpffs mount root, not bpfman-controlled and not managed
  pins. Exclude them from parity diffs on both sides.

## Background: maps and the two sharing mechanisms

A BPF map is a kernel object -- a shared table -- that programs read and
write. A program may keep a private map, or several programs may share
one. bpfman exposes sharing two independent ways:

- Explicit: a load request carries a `map_owner_id` naming an
  already-loaded program whose maps the new program reuses.
- By name: a map declared `LIBBPF_PIN_BY_NAME` is pinned at a
  well-known path and reused by anyone who loads an object declaring the
  same map. Here that path is `{bpffs}/fs/shared/<name>`.

These are independent. The by-name path is keyed by the map name and its
shared pin path: programs converge on the same map because they name it,
and it is kept alive through that shared pin and the kernel's own
references, not by any owning program. It needs no owner, and we leave
it exactly as it is.

## Normative semantics

The source of truth for this redesign is bpfman's managed-resource
contract:

- A load with no `map_owner_id` creates a private managed map set for
  that program.
- A load with explicit `map_owner_id = A` joins managed map set `A`.
  The new program must use the maps from that set for matching map
  names. It is not acceptable to report `A`'s `Map Pin Path` and
  `Map Owner ID` while secretly using newly-created private maps.
- A map set has a lifetime independent of the program that created it.
  The creator can unload first; the set remains while at least one
  managed program references it, then its pins and store row are
  garbage-collected.
- The store is the map-set reference-count implementation. The count is
  derived from program rows (`COUNT(*) WHERE map_set_id = ?`), not stored
  as a mutable integer.
- `LIBBPF_PIN_BY_NAME` is a separate, name-keyed sharing mechanism under
  `fs/shared/<name>`. It is not modelled through `map_owner_id`.
- Batch load is atomic from the caller's point of view. If one program in
  a multi-program load fails, every earlier program loaded by the same
  request is unloaded and no partial managed state is left behind.

Rust's public surface is still important: it defines the existing CLI
and API shape (`Map Owner ID`, `Map Pin Path`, `Maps Used By`,
owner-first unload, invalid-owner rejection). But Rust's internals are
not always the correct semantics. In particular, the evidence below
shows a Rust `--map-owner-id` load can report shared ownership while the
dependent program uses a different kernel map. Go already does better
for explicit ownership by loading the owner's pinned maps as
replacements; this property must be preserved.

## Verified behaviour (the proof we build on)

Driven against `bpfman/bpfman` at `target/debug/bpfman` and the local
`bin/bpfman`. Reproduced by `hack/map-sharing-demo.sh` and
`hack/map-sharing-pinned-demo.sh`.

Rust:

- Multi-program load applies the request's `map_owner_id` uniformly; with
  no owner in the request (every operator load), each program gets `None`
  and its own `/run/bpfman/fs/maps/<id>`. No implicit owner is created
  (`bpfman-api/src/bin/rpc/rpc.rs:49`, `:98`).
- Explicit `--map-owner-id A` updates Rust's map-set bookkeeping:
  shared `Map Pin Path`, reported `Map Owner ID`, `Maps Used By`,
  owner-first unload, later joins by the surviving set id, and final set
  GC. That public lifecycle shape is valid. However, Rust does not
  always reuse the owner's kernel map objects. For the
  `multi_prog_kprobe_counter` fixture, even loading the same program
  name twice (`mkp_a` then `mkp_a --map-owner-id A`) reports shared
  ownership but gives the dependent a different `mkp_a_count` kernel map
  id. That is a Rust implementation wart, not the Go target.
- Unloading owner `A` while dependent `B` exists succeeds; `A`'s shared
  map directory survives until the last user unloads. Verified: after
  unloading owner `182346`, `get program 182354` still reported
  `Map Pin Path /run/bpfman/fs/maps/182346`, `Maps Used By: 182354`, and
  the directory was still present.
- After the creating program is gone, the surviving map-set id remains a
  valid `map_owner_id` for later loads. Verified sequence:
  1. Load `A`:
     `bpfman load file --programs kprobe:mkp_a --path multi_prog_kprobe_counter.bpf.o`
     reported program `182502`, `Map Pin Path:
     /run/bpfman/fs/maps/182502`, `Map Owner ID: None`, and
     `Maps Used By: 182502`.
  2. Load `B` with `--map-owner-id 182502`:
     `bpfman load file --programs kprobe:mkp_b --path multi_prog_kprobe_counter.bpf.o --map-owner-id 182502`
     reported program `182510`, `Map Pin Path:
     /run/bpfman/fs/maps/182502`, `Map Owner ID: 182502`, and
     `Maps Used By: 182502 182510`.
  3. Unload `A`: `bpfman unload 182502` succeeded. `get program 182510`
     then reported `Map Pin Path: /run/bpfman/fs/maps/182502`,
     `Map Owner ID: 182502`, and `Maps Used By: 182510`.
  4. Load `C` with the now-ownerless set id
     `--map-owner-id 182502` succeeded, reported program `182518`, and
     showed `Map Pin Path: /run/bpfman/fs/maps/182502`,
     `Map Owner ID: 182502`, and `Maps Used By: 182510 182518`.
  So the API field is a map-set id in practice: it must name an existing
  map set, not necessarily an existing program.
- A dependent program id is not itself a valid owner unless it is also a
  map-set id. Verified: after loading `A=182486` and `B=182494` with
  `--map-owner-id 182486`, loading another program with
  `--map-owner-id 182494` failed with `map_owner_id does not exists`.

Rust evidence for the bookkeeping-only `--map-owner-id` case, using the
same object and the same program name on both loads:

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman list programs
```

Response:

```console
 Program ID  Application  Type  Function Name  Links 
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman load file --path /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o --programs kprobe:mkp_a
```

Outcome:

```console
Program ID: 183025
Map Pin Path: /run/bpfman/fs/maps/183025
Map Owner ID: None
Maps Used By: 183025
Map IDs: [258001, 258000]
```

The actual pinned counter map was the owner's map:

```console
$ sudo bpftool map show pinned /run/bpfman/fs/maps/183025/mkp_a_count
258000: array  name mkp_a_count  flags 0x0
        key 4B  value 8B  max_entries 1  memlock 312B
        btf_id 481631
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman load file --path /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o --programs kprobe:mkp_a --map-owner-id 183025
```

Outcome:

```console
Program ID: 183033
Map Pin Path: /run/bpfman/fs/maps/183025
Map Owner ID: 183025
Maps Used By: 183025 183033
Map IDs: [258007, 258009]
```

The dependent's live counter map was not the pinned owner map:

```console
$ sudo bpftool map show id 258009
258009: array  name mkp_a_count  flags 0x0
        key 4B  value 8B  max_entries 1  memlock 312B
        btf_id 481640
```

The shared directory still contained the owner's pinned maps:

```console
$ sudo ls -l /run/bpfman/fs/maps/183025
total 0
-rw-rw---- 1 root root 0 Jun 18 13:50 mkp_a_count
-rw-rw---- 1 root root 0 Jun 18 13:50 mkp_b_count
-rw-rw---- 1 root root 0 Jun 18 13:50 mkp_c_count
```

Owner-first unload succeeded and left the directory while the dependent
still existed:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman unload 183025
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman get program 183033
Program ID: 183033
Map Pin Path: /run/bpfman/fs/maps/183025
Map Owner ID: 183025
Maps Used By: 183033
Map IDs: [258007, 258009]

$ sudo ls -l /run/bpfman/fs/maps/183025
total 0
-rw-rw---- 1 root root 0 Jun 18 13:50 mkp_a_count
-rw-rw---- 1 root root 0 Jun 18 13:50 mkp_b_count
-rw-rw---- 1 root root 0 Jun 18 13:50 mkp_c_count
```

Cleanup removed the dependent, the directory, and the dependent's
private kernel map:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman unload 183033
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman list programs
 Program ID  Application  Type  Function Name  Links 

$ sudo ls -ld /run/bpfman/fs/maps/183025
ls: cannot access '/run/bpfman/fs/maps/183025': No such file or directory

$ sudo bpftool map show id 258009
Error: get map by id (258009): No such file or directory
```

Conclusion: Rust's public map-set lifetime behaviour is useful evidence;
its lack of real map replacement for this explicit-owner fixture is not
the semantic target.

This implementation, today:

- The gRPC server sets `ShareMaps: len(programs) > 1` (`server/load.go:90`)
  and `manager/load.go:492` assigns `loaded[0]`'s id as the owner of
  every later program that did not request one -- even for fully private
  maps. The CLI does not do this (`cmd/bpfman/load_file.go` leaves
  `ShareMaps` unset), so a CLI multi-program load reports
  `map_owner_id=None` for every program, like Rust.
- Unload refuses while dependents exist (`manager/unload.go`):
  "unload dependents first". The dependent's maps live under the owner's
  id-namespaced directory `{bpffs}/fs/maps/<owner_id>`, and the schema
  enforces the same with `FOREIGN KEY (map_owner_id) ... ON DELETE
  RESTRICT` (`schema.sql:169`). The dependent count is derived,
  `SELECT COUNT(*) ... WHERE map_owner_id = ?` (`stmts.go:74`), not a
  stored counter.
- An invalid `map_owner_id` is rejected today only incidentally. Naming
  a dependent (e.g. `B` after `B` joined `A`) or an absent id fails when
  the loader tries to read the shared map out of that id's directory:
  `load shared map "mkp_a_count" from owner <id>: no such file or
  directory`. Rust validates the id and returns `map_owner_id does not
  exists`. The new model must reject by an explicit `map_sets.id` check
  with a clean error, not as a side effect of a missing pin directory.
  The `get` shape the scripts assert is confirmed:
  `record.handles.{map_owner_id, map_pin_path}`.
- Explicit `map_owner_id` does real kernel-map replacement today. The
  loader reads matching map names from the owner's pinned directory and
  passes them as cilium/ebpf `MapReplacements`
  (`platform/ebpf/load.go:182-208`). This is stronger and more coherent
  than Rust's bookkeeping-only behaviour for the kprobe fixture above,
  and it must be preserved.
- `LIBBPF_PIN_BY_NAME` sharing already works correctly and independently:
  the fixture `multi_prog_kprobe_shared_pinned.bpf.o` (two kprobe
  programs sharing one pinned map), loaded as one CLI multi-program load,
  yields both programs with `map_owner_id=None` resolving to the same
  kernel map id via `{bpffs}/fs/shared/<name>`.
- The by-name mechanism has its own reference table and GC path today.
  `SharedMapPinStore` records `(map_name, program_id)` in
  `shared_map_pins`; unload calls `DeleteSharedMapPins` to find names no
  longer referenced by any program, then removes only
  `{bpffs}/fs/shared/<name>`. This is deliberately separate from
  explicit map ownership, whose pins live under `{bpffs}/fs/maps/<id>`.
  The map-set refactor must preserve that separation.

Go evidence for explicit real sharing, using the same object and program
name as the Rust proof but under the branch-local `/run/go-bpfman` root:

Command:

```console
$ sudo ./bin/bpfman program load file /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o --programs kprobe:mkp_a
```

Outcome:

```console
Program ID: 183125
Map Owner ID: None
Map Pin Path: /run/go-bpfman/fs/maps/183125
Map Dir: /run/go-bpfman/fs/maps/183125
Map 258053: Name .rodata
Map 258054: Name mkp_a_count
Prog Pin: /run/go-bpfman/fs/prog_183125
```

Command:

```console
$ sudo ./bin/bpfman program load file /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o --programs kprobe:mkp_a --map-owner-id 183125
```

Outcome:

```console
Program ID: 183132
Map Owner ID: 183125
Map Pin Path: /run/go-bpfman/fs/maps/183125
Map Dir: /run/go-bpfman/fs/maps/183125
Map 258060: Name .rodata
Map 258054: Name mkp_a_count
Prog Pin: /run/go-bpfman/fs/prog_183132
```

The dependent reused the owner's `mkp_a_count` map id `258054`. Only
`.rodata` remained private, which is expected.

Filesystem and pinned-map residue after both loads:

```console
$ sudo ls -l /run/go-bpfman/fs/maps/183125
total 0
-rw------- 1 root root 0 Jun 18 13:56 mkp_a_count
-rw------- 1 root root 0 Jun 18 13:56 mkp_b_count
-rw------- 1 root root 0 Jun 18 13:56 mkp_c_count

$ sudo bpftool map show pinned /run/go-bpfman/fs/maps/183125/mkp_a_count
258054: array  name mkp_a_count  flags 0x0
        key 4B  value 8B  max_entries 1  memlock 312B
        btf_id 481755
```

The current Go defect is lifecycle, not real sharing. Owner-first unload
still fails today:

```console
$ sudo ./bin/bpfman program unload 183125
program 183125: cannot unload program 183125: 1 dependent program(s) share its maps; unload dependents first
bpfman: error: 1 of 1 program(s) failed to unload
```

Cleanup in the currently-supported order removed both programs and the
map directory:

```console
$ sudo ./bin/bpfman program unload 183132
$ sudo ./bin/bpfman program unload 183125
$ sudo ./bin/bpfman program list

$ sudo ls -ld /run/go-bpfman/fs/maps/183125
ls: cannot access '/run/go-bpfman/fs/maps/183125': No such file or directory

$ sudo bpftool prog show id 183125
Error: get by id (183125): No such file or directory

$ sudo bpftool map show id 258054
Error: get map by id (258054): No such file or directory
```

## Target behaviour

After this work, both the CLI and the gRPC server behave as follows. The
two Go paths are identical to each other. They match Rust's public
management contract where Rust is coherent, and intentionally preserve
Go's stronger real-map-sharing semantics where Rust only updates
bookkeeping:

- A multi-program load never sets `map_owner_id` implicitly. Each program
  owns its own maps unless the request explicitly names an owner.
- `LIBBPF_PIN_BY_NAME` maps are shared by name, with every sharer
  reporting `map_owner_id`/owner = none. Program ownership is never used
  to model by-name sharing.
- `LIBBPF_PIN_BY_NAME` GC remains driven by `shared_map_pins`, not by
  `map_sets`. Map-set GC removes only `{bpffs}/fs/maps/<set_id>`;
  by-name GC removes only `{bpffs}/fs/shared/<map_name>`.
- Explicit `map_owner_id` sharing reuses the named map set and loads the
  set's pinned maps as replacements for matching map names. The owner may
  be unloaded before its dependents; the shared map set survives until
  the last user is gone, then its pins are removed and the set is
  deleted.
- The dependent count stays derived from the store. No persisted
  refcount integer is introduced.
- Multi-program load rollback remains all-or-nothing. Go already unloads
  previously-loaded programs in reverse order if a later program or the
  final store transaction fails (`manager/load.go:482-489`). Do not
  weaken this to match any Rust partial-cleanup behaviour.

## Identity model

bpfman uses the kernel program id as a program's managed-record identity.
That is a deliberate choice, not an oversight: it keeps path and identity
parity with Rust (`/fs/prog_<id>`, `/fs/maps/<id>`, `program get <kernel
id>`), it is the handle operators and `bpftool` already use, and it is
what lets the load path stay lockless (the kernel allocates the unique id,
so bpfman needs no id-allocation write point on load).

The cost is that a kernel id is not a durable identifier -- it is
kernel-owned and recyclable -- so borrowing it as a stable key is only
safe while the keyed entity does not outlive the kernel object. Programs
satisfy that; a record's life ends with its kernel program. Derived
entities that must outlive their creating program do not, and the map set
is the first such entity (it survives owner unload until the last user is
gone). That is precisely where the borrowed-id choice stops being free and
the reuse collision in the store model below appears.

For contrast, upstream Rust already owns identity where it matters: it
pins links at fresh random ids rather than kernel link ids (so a program
can attach repeatedly). Owned identity is the cleaner default; the kernel
id is the pragmatic exception we keep for programs on parity, handle, and
lockless-load grounds.

This is not a mandate to re-key programs onto bpfman-owned ids. Programs
keep the kernel id; only longer-lived derived entities such as the map
set need their own durable identity, the clean shape being a bpfman-owned
id with the kernel id stored as an attribute. Widening this into a global
program re-key is out of scope.

Sequencing matters. This branch first restores Rust parity and corrects
the bpfman-operator workaround that was introduced only because Go's
current backend cannot unload owner-first. During that parity phase, Go
keeps Rust-compatible map-set path keying (`/fs/maps/<creator kernel
id>`) so the A/B runtime layout remains directly comparable. A durable
map-set identity and durable-id-based map paths are the cleaner
long-term model, but they intentionally diverge from Rust's layout and
are deferred until after parity and operator behaviour are corrected.

## Rust identity investigation

This section records the exact evidence for the map-set identity problem
in upstream Rust. The question was whether Rust has a hidden generation,
tombstone, UUID, or durable map-set id that protects it from kernel
program id reuse. The answer from source and runtime evidence is no:
Rust keys map-set state directly on the creator's kernel program id.

Source evidence from `/home/aim/src/github.com/bpfman/bpfman`:

- `add_programs_internal` reads `map_owner_id` from the first program in
  the request and, if present, validates it by calling
  `is_map_owner_id_valid` before loading:

```rust
// bpfman/src/lib.rs:231-240
// Since all the programs are loaded from the same bytecode image
// a lot of the data is the same. This is why we can just use the
// first program to get the map_owner_id.
let map_owner_id = programs[0].get_data().get_map_owner_id()?;

// Set map_pin_path if we're using another program's maps
if let Some(map_owner_id) = map_owner_id {
    for program in programs.iter_mut() {
        let map_pin_path = is_map_owner_id_valid(root_db, map_owner_id)?;
        program.get_data_mut().set_map_pin_path(&map_pin_path)?;
    }
}
```

- `is_map_owner_id_valid` accepts an owner if and only if sled contains a
  tree named `map_<id>`, and it returns `/run/bpfman/fs/maps/<id>`:

```rust
// bpfman/src/lib.rs:1836-1848
// This function checks to see if the user provided map_owner_id is valid.
fn is_map_owner_id_valid(root_db: &Db, map_owner_id: u32) -> Result<PathBuf, BpfmanError> {
    let map_pin_path = calc_map_pin_path(map_owner_id);
    let name: &sled::IVec = &format!("{}{}", MAP_PREFIX, map_owner_id).as_bytes().into();

    if root_db.tree_names().contains(name) {
        // Return the map_pin_path
        return Ok(map_pin_path);
    }
    Err(BpfmanError::Error(
        "map_owner_id does not exists".to_string(),
    ))
}
```

- Creating a self-owned map set opens `map_<kernel_program_id>` and
  writes `maps_used_by` into that tree. `open_tree` opens an existing
  tree if it already exists; it is not a uniqueness guard:

```rust
// bpfman/src/lib.rs:1903-1911
None => {
    let db_tree = root_db
        .open_tree(format!("{}{}", MAP_PREFIX, id))
        .expect("Unable to open map db tree");

    set_maps_used_by(db_tree, vec![id])?;

    // Update this program with the updated map_used_by
    data.set_maps_used_by(vec![id])?;
```

- A dependent appends itself to the existing `map_<owner_id>` tree:

```rust
// bpfman/src/lib.rs:1881-1895
Some(m) => {
    if let Some(map) = get_map(m, root_db) {
        push_maps_used_by(map.clone(), id)?;
        let used_by = get_maps_used_by(map)?;

        // This program has no been inserted yet, so set map_used_by to
        // newly updated list.
        data.set_maps_used_by(used_by.clone())?;

        // Update all the programs using the same map with the updated map_used_by.
        for used_by_id in used_by.iter() {
            if let Some(mut program) = get(root_db, used_by_id) {
                program.get_data_mut().set_maps_used_by(used_by.clone())?;
            }
        }
```

- Unload removes the `map_<id>` tree and `/run/bpfman/fs/maps/<id>` only
  when `maps_used_by` becomes empty:

```rust
// bpfman/src/lib.rs:1943-1967
fn delete_map(root_db: &Db, id: u32, map_owner_id: Option<u32>) -> Result<(), BpfmanError> {
    let index = match map_owner_id {
        Some(i) => i,
        None => id,
    };

    if let Some(map) = get_map(index, root_db) {
        let mut used_by = get_maps_used_by(map.clone())?;

        if let Some(index) = used_by.iter().position(|value| *value == id) {
            used_by.swap_remove(index);
        }

        clear_maps_used_by(map.clone());
        set_maps_used_by(map.clone(), used_by.clone())?;

        if used_by.is_empty() {
            let path: PathBuf = calc_map_pin_path(index);
            // No more programs using this map, so remove the entry from the map list.
            root_db
                .drop_tree(MAP_PREFIX.to_string() + &index.to_string())
                .expect("unable to drop maps tree");
            remove_dir_all(path)
                .map_err(|e| BpfmanError::Error(format!("can't delete map dir: {e}")))?;
```

- The map pin path is exactly `/run/bpfman/fs/maps/<id>`:

```rust
// bpfman/src/lib.rs:1984-1989
// map_pin_path is a the directory the maps are located. Currently, it
// is a fixed bpfman location containing the map_index, which is a ID.
// The ID is either the programs ID, or the ID of another program
// that map_owner_id references.
pub(crate) fn calc_map_pin_path(id: u32) -> PathBuf {
    PathBuf::from(format!("{RTDIR_FS_MAPS}/{}", id))
}
```

Live Rust run, from a clean Rust program list. The binary was
`/home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman`.

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman list programs
```

Response:

```console
 Program ID  Application  Type  Function Name  Links 
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman load file --programs kprobe:mkp_a --path /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o
```

Response:

```console
 Bpfman State                                                                                                
 BPF Function:  mkp_a                                                                                        
 Program Type:  kprobe                                                                                       
 Path:          /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o 
 Global:        None                                                                                         
 Metadata:      None                                                                                         
 Map Pin Path:  /run/bpfman/fs/maps/182946                                                                   
 Map Owner ID:  None                                                                                         
 Maps Used By:  182946                                                                                       
 Links:         None                                                                                         

 Kernel State                                               
 Program ID:                       182946                   
 BPF Function:                     mkp_a                    
 Kernel Type:                      probe                    
 Loaded At:                        2026-06-18T13:21:53+0100 
 Tag:                              6d8307ace14667ca         
 GPL Compatible:                   true                     
 Map IDs:                          [257955, 257956]         
 BTF ID:                           481539                   
 Size Translated (bytes):          208                      
 JITted:                           true                     
 Size JITted:                      133                      
 Kernel Allocated Memory (bytes):  4096                     
 Verified Instruction Count:       19                       
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman load file --programs kprobe:mkp_b --path /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o --map-owner-id 182946
```

Response:

```console
 Bpfman State                                                                                                
 BPF Function:  mkp_b                                                                                        
 Program Type:  kprobe                                                                                       
 Path:          /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o 
 Global:        None                                                                                         
 Metadata:      None                                                                                         
 Map Pin Path:  /run/bpfman/fs/maps/182946                                                                   
 Map Owner ID:  182946                                                                                       
 Maps Used By:  182946                                                                                       
                182954                                                                                       
 Links:         None                                                                                         

 Kernel State                                               
 Program ID:                       182954                   
 BPF Function:                     mkp_b                    
 Kernel Type:                      probe                    
 Loaded At:                        2026-06-18T13:22:02+0100 
 Tag:                              6d8307ace14667ca         
 GPL Compatible:                   true                     
 Map IDs:                          [257962, 257963]         
 BTF ID:                           481548                   
 Size Translated (bytes):          208                      
 JITted:                           true                     
 Size JITted:                      133                      
 Kernel Allocated Memory (bytes):  4096                     
 Verified Instruction Count:       19                       
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman unload 182946
```

Response:

```console
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman get program 182946
```

Response:

```console
Error: get error: An error occurred. Program 182946 does not exist
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman get program 182954
```

Response:

```console
 Bpfman State                                                                                                
 BPF Function:  mkp_b                                                                                        
 Program Type:  kprobe                                                                                       
 Path:          /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o 
 Global:        None                                                                                         
 Metadata:      None                                                                                         
 Map Pin Path:  /run/bpfman/fs/maps/182946                                                                   
 Map Owner ID:  182946                                                                                       
 Maps Used By:  182954                                                                                       
 Links:         None                                                                                         

 Kernel State                                               
 Program ID:                       182954                   
 BPF Function:                     mkp_b                    
 Kernel Type:                      probe                    
 Loaded At:                        2026-06-18T13:22:02+0100 
 Tag:                              6d8307ace14667ca         
 GPL Compatible:                   true                     
 Map IDs:                          [257962, 257963]         
 BTF ID:                           481548                   
 Size Translated (bytes):          208                      
 JITted:                           true                     
 Size JITted:                      133                      
 Kernel Allocated Memory (bytes):  4096                     
 Verified Instruction Count:       19                       
```

Command:

```console
$ sudo ls -la /run/bpfman/fs/maps/182946
```

Response:

```console
total 0
drwxr-xr-x 2 root root 0 Jun 18 13:21 .
drwxr-xr-x 3 root root 0 Jun 18 13:21 ..
-rw-rw---- 1 root root 0 Jun 18 13:21 mkp_a_count
-rw-rw---- 1 root root 0 Jun 18 13:21 mkp_b_count
-rw-rw---- 1 root root 0 Jun 18 13:21 mkp_c_count
```

Command:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman load file --programs kprobe:mkp_c --path /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o --map-owner-id 182946
```

Response:

```console
 Bpfman State                                                                                                
 BPF Function:  mkp_c                                                                                        
 Program Type:  kprobe                                                                                       
 Path:          /home/aim/src/github.com/frobware/go-bpfman/e2e/testdata/bpf/multi_prog_kprobe_counter.bpf.o 
 Global:        None                                                                                         
 Metadata:      None                                                                                         
 Map Pin Path:  /run/bpfman/fs/maps/182946                                                                   
 Map Owner ID:  182946                                                                                       
 Maps Used By:  182954                                                                                       
                182969                                                                                       
 Links:         None                                                                                         

 Kernel State                                               
 Program ID:                       182969                   
 BPF Function:                     mkp_c                    
 Kernel Type:                      probe                    
 Loaded At:                        2026-06-18T13:22:34+0100 
 Tag:                              6d8307ace14667ca         
 GPL Compatible:                   true                     
 Map IDs:                          [257974, 257972]         
 BTF ID:                           481565                   
 Size Translated (bytes):          208                      
 JITted:                           true                     
 Size JITted:                      133                      
 Kernel Allocated Memory (bytes):  4096                     
 Verified Instruction Count:       19                       
```

Cleanup commands:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman unload 182954
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman unload 182969
```

Responses:

```console
```

Final cleanup verification:

```console
$ sudo /home/aim/src/github.com/bpfman/bpfman/target/debug/bpfman list programs
```

```console
 Program ID  Application  Type  Function Name  Links 
```

```console
$ sudo ls -ld /run/bpfman/fs/maps/182946
```

```console
ls: cannot access '/run/bpfman/fs/maps/182946': No such file or directory
```

```console
$ sudo bpftool prog show id 182946
```

```console
Error: get by id (182946): No such file or directory
```

```console
$ sudo bpftool prog show id 182954
```

```console
Error: get by id (182954): No such file or directory
```

```console
$ sudo bpftool prog show id 182969
```

```console
Error: get by id (182969): No such file or directory
```

What this proves:

- Rust has no hidden durable map-set identity in this path. The durable
  state is a sled tree named `map_<kernel_program_id>` plus pins under
  `/run/bpfman/fs/maps/<kernel_program_id>`.
- A map set can outlive the kernel program whose id names it. After
  `unload 182946`, `get program 182946` fails, but
  `/run/bpfman/fs/maps/182946` remains and `182946` is still accepted as
  a valid `--map-owner-id`.
- The id has changed meaning: while the owner program is live, `182946`
  is both a program id and a map-set id. After owner unload, it is only a
  map-set id.
- The source has no visible protection against a later kernel program id
  reuse. If the kernel later assigns `182946` to an unrelated new
  program while `map_182946` still exists, Rust's self-owned load path
  calls `open_tree("map_182946")`, which opens the existing tree rather
  than failing as a uniqueness violation. That is worse than a clean
  collision: it risks mixing a new unrelated program into old map-set
  bookkeeping.
- I did not force kernel id reuse empirically. The evidence is source
  proof plus the live proof of the precondition: Rust lets map-set state
  outlive the creator while keying it solely by that creator's recyclable
  kernel id.

Design implication for Go: do not copy Rust's dangerous failure mode.
For the parity phase we keep Rust-compatible path keying, but Go must
fail closed if a self-owned load tries to create a map set whose id is
already live. Self-owned map-set creation must be an `INSERT` that fails
on primary-key collision; it must never be implemented as upsert,
ensure-exists, or open-or-create. That converts Rust's likely silent
merge into a clean load error while preserving the path layout needed for
this branch's A/B proof loop.

Worked example -- a recycled program id while a map set is alive:

1. Program 100 loads; bpfman creates map set 100 at
   `{bpffs}/fs/maps/100`.
2. A second program joins map set 100 (`--map-owner-id 100`).
3. Program 100 unloads, but the joiner still references the set, so map
   set 100 stays alive.
4. Later the kernel reuses id 100 for an unrelated new program.

Now `100` means two things at once: the surviving map set 100 and the new
program 100. Rust's `open_tree` opens the existing set, so the new program
is silently folded into the old set's bookkeeping -- two unrelated
workloads sharing a map directory and a lifetime, with no error. Go fails
closed instead: the new self-owned load needs to create map set 100, hits
the primary-key collision, and returns a clear error naming the cause.
The surviving set and its users keep working untouched; only the new
program is refused, and only as a self-owned load while map set 100 is
still alive. The durable-identity follow-up removes the collision outright
by giving the set an id that cannot clash with a kernel program id.

Why this matters in practice (Rust's exposure is not just untidy
bookkeeping):

- Different map names: Rust pins the new program R's maps into the
  surviving `/fs/maps/100` and records R as a user of the old set. A
  later unload of R can then decide it is the last user and remove
  `/fs/maps/100` while B is still running -- tearing out another
  workload's managed pins and breaking later joins, CSI access,
  observability, and cleanup.
- Overlapping map names: the pin fails because the file already exists,
  but because R loaded with no owner Rust believes it owns
  `/fs/maps/100`, so its load-failure cleanup path can delete the
  surviving set's pins that B depends on -- a failed, unrelated load
  destroying another workload's map set.

Either way an unrelated program can corrupt or destroy another's managed
map state, not merely confuse a counter. Go's fail-closed insert -- refuse
before R can pin a map or mutate any map-set state -- is what prevents
both the silent merge and the destructive cleanup.

The full robustness fix is durable map-set identity plus map paths keyed
by that durable id. That is a deliberate follow-up hardening, not part of
the parity slice, because it changes the on-disk layout and user-visible
`Map Pin Path` away from Rust.

## Store model

We replace the owner self-reference with a first-class map-set entity, so
a shared map set has a lifetime independent of the program that created
it.

Remove from `managed_programs`: the `map_owner_id` column, its
`ON DELETE RESTRICT` foreign key, and the `map_pin_path` column.

Add a table:

    CREATE TABLE map_sets (
        id        INTEGER PRIMARY KEY,  -- the creating program's kernel id;
                                        -- a stable label that outlives the creator
        pin_path  TEXT NOT NULL,        -- {bpffs}/fs/maps/<id>
        created_at TEXT NOT NULL
    ) STRICT;

Add to `managed_programs`:

    map_set_id INTEGER NOT NULL
        REFERENCES map_sets(id) ON DELETE RESTRICT

Rules:

- Loading a program with no `map_owner_id` creates a new `map_sets` row
  whose `id` is the program's own kernel id and whose `pin_path` is
  `{bpffs}/fs/maps/<id>`; the program's `map_set_id` is that id. This is
  the program owning its own maps, and the path matches Rust's
  `/run/bpfman/fs/maps/<owner_id>`.
- Loading with `map_owner_id = A` joins map set `A`. The id must exist
  in `map_sets`; the creating program may already have unloaded, and a
  dependent program id is rejected unless it is itself a map-set id. This
  matches Rust: `map_owner_id` is exposed as an owner id, but the
  lifecycle referent is the surviving map set. Unlike Rust's
  bookkeeping-only behaviour for the kprobe fixture above, Go must still
  load the set's pinned maps as replacements for matching map names.
- Unloading a program deletes its program row (links cascade as today),
  then garbage-collects its map set: if
  `SELECT COUNT(*) FROM managed_programs WHERE map_set_id = ?` is zero,
  remove the pins under the set's `pin_path` and delete the `map_sets`
  row. Deleting a program never cascades into its map set, so the set
  outlives the owning program until the last user is gone.

The map-set `id` deliberately reuses the creating program's id in this
parity phase, so the on-disk layout and reported owner id match Rust's
and an A/B comparison against the Rust binary is direct. This inherits
Rust's id-reuse exposure at the path level, but Go must narrow the
failure mode: a reused id colliding with a surviving map set is a hard
load error, not a silent join. Implement this by inserting a fresh
`map_sets` row for self-owned loads and treating primary-key collision as
fatal. Do not use upsert or open-or-create semantics for self-owned set
creation.

After parity and the operator unload-order correction are complete, the
follow-up hardening is to replace this borrowed identity with a durable
bpfman-owned map-set id and to key map paths by that durable id. That
will intentionally diverge from Rust's current layout and needs its own
design, proof, and migration/no-migration decision.

The gRPC and CLI APIs keep their `map_owner_id` field; it names an
existing map set. For the common direct A/B case this is also the
creating program's id, so the user-facing shape still matches Rust. The
server simply stops defaulting it.

Although `map_owner_id` and `map_pin_path` leave `managed_programs`,
the public record shape remains unchanged. Store reads join
`managed_programs.map_set_id` to `map_sets` and populate
`record.handles.map_pin_path` from `map_sets.pin_path`; they populate
`record.handles.map_owner_id` as `nil` when
`program_id == map_set_id`, otherwise as `map_set_id`. This preserves
the CLI, JSON, and gRPC surfaces while making the lifetime model correct.

One public-shape point remains to settle before Phase 2. Rust reports
`Maps Used By` as part of the observed map-set state. The Go protobuf
already has `ProgramInfo.map_used_by`, but this plan currently only
specifies `map_owner_id` and `map_pin_path` plumbing. Decide explicitly
whether Go must derive and expose the users of a map set
(`SELECT program_id FROM managed_programs WHERE map_set_id = ?`) or
whether `Maps Used By` is Rust CLI display detail outside this branch's
public parity target. If it is out of scope, remove it from tests and
proof language; if it is in scope, add response, CLI, and JSON
assertions.

## Fresh-session implementation hazards

These are the known traps that are easy to miss when implementing from a
clean context:

- Kernel program-id reuse is a known Rust identity bug that this parity
  phase does not fully fix because the full fix would change the
  `/fs/maps/<id>` layout. Go must still be safer than Rust: self-owned
  map-set creation is insert-only and fails loudly on id collision,
  never open-or-create.
- The existing unload path removes `BPFFS().MapPinDir(programID)` on
  every unload. Under map sets this is wrong for owner-first unload:
  `/fs/maps/<set_id>` must survive until the last user leaves. Phase 2
  must replace unconditional per-program map-dir removal with map-set GC
  by derived user count.
- The existing writer-lock model still matters. Mutating operations run
  under the global writer flock, but `Load` is deliberately lockless
  unless it touches shared state. Today `LIBBPF_PIN_BY_NAME` loads take
  the writer flock. A load with explicit `map_owner_id` must do the same,
  because it joins shared map-set state that a concurrent unload may be
  garbage-collecting. Set creation with no owner and no PinByName maps
  can remain lockless.
- Shared PinByName cleanup must remain separate from map-set GC.
  `shared_map_pins` has `ON DELETE CASCADE` to `managed_programs`; if
  the program row is deleted before orphaned shared pins are computed,
  the evidence needed to remove `/fs/shared/<name>` disappears.
- CSI selector tests should start at manager level around
  `FindLoadedProgramByMetadata`; full CSI volume machinery is not needed
  to prove the first regression. Escalate to the heavier path only if the
  manager-level proof is insufficient.
- The current sqlite `schemaVersion` is 15. The map-set schema change
  should bump it to 16 unless another schema change lands first.

## Implementation plan

Each phase: observe Rust, write the failing test, make it pass, prove it.
Do not skip the red step. If a proposed test is already green, it is not
proving the defect and must be sharpened. Prefer fakeKernel tests for
manager/store state-machine behaviour, store tests for schema and
derived-count invariants, and `.bpfman` scripts for real-kernel proof.
Use direct Rust/Go binary runs and the diffable runtime trees to validate
the behavioural contract.

### Phase 1: stop fabricating ownership and fix CSI selection

- Red: a gRPC server-level test in the `e2e/grpc` harness, which runs a
  real `bpfman serve` and drives it as a client (a new test function, not
  an extension of the single-program lifecycle helper). It mirrors what a
  #493-reverted operator does: issue one `LoadRequest` carrying three
  `Info` entries from the private-map `multi_prog_kprobe_counter` fixture
  with no request-level `map_owner_id` (the operator's `getLoadRequest`
  shape). Assert three programs come back, none carrying a `map_owner_id`
  in the response, and `Get` each to confirm the absence is persisted
  store state, not just response formatting. Then unload in the order
  returned by `LoadResponse.Programs` (which follows `Info` order) -- the
  first returned program first -- and assert success. Load-order unload
  is exactly what bpfman-operator PR #493 reversed to work around this
  bug; today it fails on the fabricated owner's dependents-first guard,
  so this is the load-bearing regression. The CLI path does not fabricate
  ownership today, so only the gRPC path can exhibit it. This in-tree
  test is the equivalent of reverting #493, provable in this repo's CI
  (`ci-test-e2e-grpc`) without a cluster.
- Red/green proof for the CLI path: a `.bpfman` script loads two
  private-map programs and unloads owner-then-dependent order (load
  order) successfully. This locks CLI/Rust parity and guards against
  regressing the already-correct CLI behaviour, but it is not a
  substitute for the manager/server gRPC test above.
- Red: a CSI/metadata lookup test for a multi-program application after
  forced ownership is removed. The current Go selector reports
  `ErrMultipleMapOwners` because multiple matching rows legitimately
  have `MapOwnerID == nil`; Rust publishes from the first metadata match
  instead.
- Green: remove the `ShareMaps: len(programs) > 1` forcing in
  `server/load.go` and the implicit-owner assignment in
  `manager/load.go`. Loading no longer sets `MapOwnerID` unless the
  request supplied one. In the same slice, rework
  `manager.FindLoadedProgramByMetadata` and its CSI caller. That
  helper currently assumes a multi-program application has exactly one
  `MapOwnerID == nil` program and uses that row's map pin path for CSI.
  That assumption is only true because the gRPC load path fabricates a
  first-program owner. Once server loads stop fabricating ownership, a
  multi-program application legitimately has multiple no-owner programs,
  and the current helper would report `ErrMultipleMapOwners` instead of
  publishing the maps.

  Rust does not do this owner-selection step. In
  `bpfman-api/src/bin/rpc/storage.rs`, CSI reads
  `csi.bpfman.io/program` and `csi.bpfman.io/maps`, lists programs, picks
  the first loaded program whose `bpfman.io/ProgramName` metadata matches
  the requested program name, takes that program's `map_pin_path`, and
  loads every requested map from that one directory. If a requested map
  is absent, Rust fails with `map <name> not found in <program>'s pinned
  maps`.

  Go must match that contract: for CSI, find the first reconciled program
  matching `bpfman.io/ProgramName`, use its `record.handles.map_pin_path`
  (derived from its `map_set_id`), and attempt to publish all requested
  maps from that directory. Do not search for "the map owner among
  matches", and do not spread a single CSI request across multiple
  programs' map directories. Missing maps are a clean CSI error, not a
  reason to keep the fabricated ownership model.
- Existing tests asserting the fabricated behaviour are rewritten to the
  new contract.
- Proof: Go and Rust multi-program gRPC/CLI loads with no explicit owner
  both produce no `map_owner_id`; Go and Rust unload successfully in load
  order; CSI publishes maps from the first metadata match instead of
  failing with `ErrMultipleMapOwners`; `/run/go-bpfman` and
  `/run/bpfman` show equivalent managed map directories for the exercised
  scenario.

### Phase 2: map-set entity and owner-first unload

- Red: a fakeKernel manager test that loads `A`, loads `B` with
  `map_owner_id = A`, unloads `A` first, and asserts success with `B`
  still bound to the surviving map set; a second assert that unloading
  `B` then deletes the set. A `.bpfman` script proves the same against
  the real kernel: owner-first unload succeeds, the dependent keeps
  working, the shared map persists, and the set's pins disappear only
  after the last user unloads. Both fail today (the guard and the FK).
- Green: the `map_sets` schema above; load creates or joins a set; unload
  deletes the program then GCs the set by derived count; pin paths come
  from the set. Remove the `map_owner_id` column, its FK, and the
  dependents-first guard in `manager/unload.go`.
- Self-owned map-set creation is insert-only. A primary-key collision on
  `map_sets.id` means the kernel reused an id while an old map set with
  that path is still alive; return a clean load error that names the
  cause (a reused kernel id collided with a surviving map set), so the
  failure is diagnosable rather than a bare constraint violation. Do not
  upsert, ensure, or otherwise join an existing set for a no-owner load.
- Prove the fail-closed rule with a fakeKernel test, not a real-kernel
  one. Forcing actual BPF id reuse is unreliable and kernel-dependent, so
  it is a poor e2e target; the fake kernel controls allocated ids
  deterministically and models the precondition directly. Sequence: load
  A (fake returns id 100); load B `--map-owner-id 100` (fake returns
  101); unload A, leaving map set 100 alive because B still references
  it; load an unrelated R with no owner and have the fake return id 100
  again; assert the R load fails cleanly because map set 100 already
  exists. Assert the durable state, not just the error: R is not
  persisted, B and map set 100 are unchanged, and nothing removed
  `/fs/maps/100` from the fake bpffs model. This proves bpfman handles
  reuse safely without having to prove the kernel reuses ids, and it
  lives with the Phase 2 map-set tests as a store/lifecycle invariant.
- Practicality (confirmed against the fakeKernel): this is cheap. The
  fake assigns program ids from a `nextID` counter initialised to 100
  (`fake_kernel_test.go`), so forcing reuse is a one-liner --
  `fk.nextID.Store(100)` before R's load, the same call the constructor
  already uses. The collision itself is enforced by the sqlite `map_sets`
  primary key in the manager's load path, not by the fake, so the fake
  only has to return the reused id; the load-bearing assertions (clean
  failure, R not persisted, B and set 100 unchanged) are pure store-level
  and need no kernel-reuse realism. The only part needing more is the
  `/fs/maps/100`-not-removed assertion, which depends on the fake
  modelling map-dir presence; it is optional, because the primary key is
  the real guard.
- Replace the existing unconditional
  `removeProgramMapsPins(BPFFS().MapPinDir(programID))` behaviour. A
  program unload must not remove `/fs/maps/<set_id>` while any other
  program still references that set. Map-set pin removal happens only in
  the zero-users GC path.
- Extend the existing load writer-lock condition. A load that declares
  `LIBBPF_PIN_BY_NAME` maps already acquires the writer flock because it
  touches shared map state. A load with explicit `map_owner_id` must also
  acquire it, because it joins a map set that a concurrent unload may be
  deleting. A no-owner, non-PinByName load can stay lockless.
- Preserve the existing `LIBBPF_PIN_BY_NAME` cleanup ordering or replace
  it with an equivalent explicit mechanism. Today `shared_map_pins` has
  `ON DELETE CASCADE` to `managed_programs`, so `cleanupSharedMapPins`
  must run before the program row is deleted; otherwise the rows vanish
  before `DeleteSharedMapPins` can compute which shared pins became
  orphaned. The map-set unload change must not accidentally make
  `/fs/shared/<name>` pins immortal or remove them through map-set GC.
- No migration: recreate the schema at the new version and delete old
  databases during development/test setup. Do not preserve
  `managed_programs.map_owner_id` or `managed_programs.map_pin_path` as
  compatibility columns. The current schema version is 15; bump to 16
  for this schema change unless another schema change lands first.
- Proof: Go and Rust both allow owner-first unload, both allow a later
  load to join the surviving set id, both reject dependent-as-owner and
  absent set ids intentionally, and both GC the set pins after the last
  user unloads.

### Phase 3: reconcile existing tests and scripts

- Update store and manager tests that referenced the old model:
  `platform/store/sqlite/sqlite_test.go`
  (`TestMapOwnership_CountDependentPrograms` and siblings),
  `manager/delete_test.go`, `manager/reap_test.go`,
  `e2e/dispatcher_tc_test.go`.
- Verify and, if needed, correct the `.bpfman` scripts
  `TestProgramDelete_SharedMapFanOut` and `TestTC_PinByNameMapSharing`
  against the new behaviour; the by-name script must remain green
  unchanged in intent.

### Phase 4: operator follow-up (separate repo)

- The `e2e/grpc` load-order test in Phase 1 is the in-tree equivalent of
  a #493-reverted operator, so the fix is provable here without a
  cluster. The authoritative end-to-end gate is the bpfman-operator
  integration suite run on KIND against this backend with #493 reverted:
  go-bpfman must pass it.
- Revert bpfman-operator PR #493 so both reconcilers unload in their
  natural order. This lands after the bpfman change, never before, or
  forward-order unload would leak against the old backend.

## Pre-merge cleanup checklist

This branch intentionally carries diagnostic scaffolding to make Go/Rust
A/B work easy. Before merging, make these decisions explicitly:

- Revert the temporary `/run/go-bpfman` default runtime root; upstream
  bpfman's canonical default is `/run/bpfman`.
- Decide whether the A/B helper scripts belong in-tree permanently. If
  kept, they must be documented as developer proof tools, not production
  workflow.
- Decide whether the additional PinByName fixture is production test
  data or branch-only scaffolding.
- Verify no old-store compatibility shims or migration paths were added;
  the accepted policy for this work is recreate-at-new-schema.
- Re-run the Rust/Go A/B proofs from clean runtime trees after removing
  temporary diagnostics, so the merge candidate itself is what was
  proven.

## Test harness

- fakeKernel manager tests for the load/unload state machine and map-set
  GC.
- Store tests for the `map_sets` schema, the join-on-`map_owner_id` path,
  and derived-count GC.
- Store tests must cover the no-compatibility decision: the new schema
  has `map_sets`, `managed_programs.map_set_id`, and no
  `managed_programs.map_owner_id` or `managed_programs.map_pin_path`
  columns.
- `.bpfman` scripts, run against a real kernel, for: a private-map
  multi-program load unloaded in load order; explicit owner-first unload
  with a surviving dependent; the `multi_prog_kprobe_shared_pinned`
  by-name sharing case.
- PinByName regression tests must prove non-interference with map sets:
  two PinByName users still share the same kernel map object under
  `/fs/shared/<name>`, both report `map_owner_id=None`, unloading one
  user leaves the shared pin present, unloading the last user removes the
  shared pin, and explicit map-set GC never removes `/fs/shared/<name>`.
- CSI/metadata lookup tests for multi-program loads after forced
  ownership is removed. The test must cover the current broken Go
  behaviour: with multiple matching no-owner programs, the old
  `FindLoadedProgramByMetadata` path reports `ErrMultipleMapOwners`
  because it expects exactly one no-owner row. The green behaviour
  matches Rust: pick the first reconciled metadata match, use its
  `record.handles.map_pin_path`, publish all requested maps from that
  directory, and fail cleanly if any requested map is missing there.
- Explicit `map_owner_id` tests must not assert equal kernel map IDs for
  `mkp_a` and `mkp_b`: those programs use different map names. For real
  explicit-sharing proof, load the same program name twice (`mkp_a`,
  then `mkp_a --map-owner-id A`) and assert the dependent reuses the
  owner's non-internal map id for `mkp_a_count`. Also assert the shared
  map-set contract (`map_pin_path`, `map_owner_id`, owner-first unload,
  later join, and GC; also `Maps Used By` if the public-shape decision
  above makes it in scope). Keep separate `LIBBPF_PIN_BY_NAME` tests for
  the name-keyed `/fs/shared/<name>` mechanism.
- `hack/map-sharing-demo.sh` and `hack/map-sharing-pinned-demo.sh` re-run
  green at each phase, and A/B against the Rust binary confirms parity.

### .bpfman script sketches

The script suite must cover both the happy path and the rejection path.
We have historically under-tested the latter; map ownership needs both,
because accepting the wrong id silently creates the wrong lifetime model.

The lifecycle sketches below use `mkp_a`, `mkp_b`, and `mkp_c` to prove
map-set ownership, owner-first unload, later join, and GC. Because those
programs use different map names, they do not prove real kernel-map
replacement. Add a separate explicit-sharing script or e2e using
`mkp_a` twice and assert the second program reuses the first program's
`mkp_a_count` kernel map id. That is the semantic proof Go must keep
even though Rust fails it for this fixture.

Happy path: `TestProgramLoad_MapSetSurvivesCreatorUnload.bpfman`

```bpfman
# -*- mode: bpfman -*-
#
# Managed map-set contract: explicit map sharing is keyed by the map-set
# id. The creating program may unload first; the set remains usable while
# any dependent still references it, and a later program may join the
# same set by naming the original id.

import ../lib.bpfman

let obj = "testdata/bpf/multi_prog_kprobe_counter.bpf.o"

guard load_a <- bpfman program load file $obj --programs kprobe:mkp_a
let a = $load_a.programs[0].record.program_id

guard load_b <- bpfman program load file $obj --programs kprobe:mkp_b --map-owner-id $a
let b = $load_b.programs[0].record.program_id

assert $load_b.programs[0].record.handles.map_owner_id == $a
assert $load_b.programs[0].record.handles.map_pin_path =~ "/maps/${a}$"

# Owner-first unload is the key lifecycle behaviour.
guard _ <- bpfman program unload $a

guard get_b <- bpfman program get $b
assert $get_b.record.handles.map_owner_id == $a
assert $get_b.record.handles.map_pin_path =~ "/maps/${a}$"

# The original owner program is gone, but its map-set id remains a
# valid map_owner_id while B still uses the set.
guard load_c <- bpfman program load file $obj --programs kprobe:mkp_a --map-owner-id $a
let c = $load_c.programs[0].record.program_id

assert $load_c.programs[0].record.handles.map_owner_id == $a
assert $load_c.programs[0].record.handles.map_pin_path =~ "/maps/${a}$"

guard _ <- bpfman program unload $b
guard get_c <- bpfman program get $c
assert $get_c.record.handles.map_owner_id == $a
assert $get_c.record.handles.map_pin_path =~ "/maps/${a}$"

# Last user leaves; the map set is garbage-collected.
guard _ <- bpfman program unload $c
assert absent bpffs "fs/maps/${a}"
```

Unhappy path: `TestProgramLoad_MapOwnerIDMustNameMapSet.bpfman`

```bpfman
# -*- mode: bpfman -*-
#
# Managed map-set contract: map_owner_id must name an existing map set.
# A dependent program id is not enough, even though that program exists,
# and a random id is rejected too.

import ../lib.bpfman

let obj = "testdata/bpf/multi_prog_kprobe_counter.bpf.o"

guard load_a <- bpfman program load file $obj --programs kprobe:mkp_a
let a = $load_a.programs[0].record.program_id
defer bpfman program unload $a

guard load_b <- bpfman program load file $obj --programs kprobe:mkp_b --map-owner-id $a
let b = $load_b.programs[0].record.program_id
defer bpfman program unload $b

# B exists, but B did not create a map set. Rust rejects this with
# "map_owner_id does not exists"; Go must reject it too.
assert fail bpfman program load file $obj --programs kprobe:mkp_a --map-owner-id $b

# A completely absent map-set id is also rejected.
assert fail bpfman program load file $obj --programs kprobe:mkp_a --map-owner-id 429496729
```

The concrete runner may need `bpffs`/filesystem assertion helpers added
or replaced with an existing shell guard. Where the runner can inspect
stderr, the unhappy-path assertions should also check for the intentional
validation message (`map_owner_id does not exist(s)`) rather than merely
any load failure, because current Go already fails these cases by
accident when a pin path is missing. The behaviour asserted above is
non-negotiable: owner-first unload works, later joins by the surviving
set id work, dependent-as-owner is refused intentionally, and nonexistent
set ids are refused intentionally.

## Decisions

- The shared map set keeps the creating program's id as its identity, so
  paths and reported owner ids match Rust and the A/B comparison is
  direct.
- The dependent count remains derived from the store; no persisted
  counter is added.
- `LIBBPF_PIN_BY_NAME` sharing is unchanged: it is the correct mechanism
  for by-name sharing and is never modelled as program ownership. Its
  existing `shared_map_pins` reference tracking remains separate from
  `map_sets`.
