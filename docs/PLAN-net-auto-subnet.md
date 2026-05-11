# Plan: auto-subnet allocation for `net veth-pair`

## 1. What this document is

A working design memo for adding pool-allocated addresses to the
`net` builtin so dispatcher tests can run in parallel without
hand-coordinating subnets. Sibling document to
`PLAN-net-builtin.md`; the architectural framing (deep module,
script chooses what matters, builtin owns mechanism) carries over
verbatim. Retire this file once the implementation ships and the
behaviour lands in `REPL-REDESIGN.md`.

Read after `PLAN-net-builtin.md`. The pool design borrows
invariants from the Go e2e suite's `vethAddrPool` (see
`e2e/helpers.go`); the novel work here is making them
cross-process and folding the result into the script-facing
surface.

## 2. Why this exists

The migrated dispatcher scripts in `e2e/new/` all assign
`198.51.100.1/32` to their host-side veth and `198.51.100.2/32`
to their peer side. The veth and netns names vary per script but
the addresses collide: two scripts in parallel both
`ip addr add 198.51.100.1/32` and both
`ip route add 198.51.100.2/32 dev vea-{suffix}`, leaving ARP and
reply-path routing ambiguous. Run two in parallel with GNU
parallel and counters silently bleed between tests.

The Go e2e suite solved this with a 127-slot pool over
`198.51.100.0/24`, paired /32s, FIFO with cooldown, and slot
provenance for leak attribution. It works for `go test` because
every subtest runs in one process. The shell corpus runs each
script as its own process, so the pool needs cross-process
coordination.

The deeper point: the addresses are not part of test intent.
Dispatcher tests care about a functioning isolated topology,
deterministic packet flow, a stable attach surface, and no
cross-test interference. They do not care which /30 they get.
Encoding addresses in the script leaks an implementation detail
the test should never have to express.

## 3. Proposed surface

The canonical fixture line becomes three required flags:

    guard pair <- net veth-pair \
        --ns=bpfsh-mtc \
        --host-link=vea-mtc \
        --peer-link=veb-mtc
    defer net release $pair

`--host-addr` and `--peer-addr` become optional overrides for
the explicit-address case. They are mutually required: pass both
or neither. Absent both means auto mode; present both means
explicit mode. The `--no-routes` flag is removed; it was only
meaningful when the auto path used paired /32s.

## 4. Two operational modes

Auto mode (default). The pool acquires a slot in `[1, 64]` over
`198.51.100.0/24`. /30 layout: slot n has base `4*(n-1)`; host
address `4*(n-1)+1`; peer address `4*(n-1)+2`; broadcast at
`4*(n-1)+3`. The connected route is automatic via the kernel;
net adds no explicit routes. NetPair carries the slot index and
an open file descriptor on the slot's lockfile, both
unexported.

Explicit mode. The caller passes `--host-addr=CIDR` and
`--peer-addr=CIDR`. net assigns the addresses to the veth ends
and stops; no route management. NetPair's slot is zero; the
lockfile field is nil. The caller takes responsibility for any
routing the topology needs. Raw `ip route add` is the documented
escape hatch from `PLAN-net-builtin.md` section 4.

The asymmetry between the two modes is intentional. Auto mode
owns a contract: parallel-safe, route-clean, self-managing.
Explicit mode is the escape hatch for the test that needs a
specific address and is willing to do its own routing. Neither
mode does "some routes but not others"; the previous
`--no-routes` flag's mid-state goes away.

## 5. /30 versus paired /32s

The original `net veth-pair` migration used paired /32s with two
symmetric `ip route add` calls per side because that was what
the pre-migration scripts did. Once the builtin owns the
topology, /30 is materially simpler: the kernel adds the
connected route automatically, two fewer `ip` invocations per
setup, no symmetric route writes, no teardown artefacts beyond
the link and the netns. The connected-route behaviour is how
Linux is designed to work; the /32 plus manual route pattern was
a workaround.

The auto-path /30 inherits all of this. The explicit path is
also liberated: if the caller gives /24-sized addresses the
kernel's connected route works the same way; if they give /32s
they need their own routes, and that is signalled clearly by
the API doing nothing magical for them.

## 6. Pool layout

64 slots over `198.51.100.0/24`. Slot 1 has host `198.51.100.1`
and peer `198.51.100.2`. Slot 2 has host `198.51.100.5` and
peer `198.51.100.6`. Slot 64 has host `198.51.100.253` and peer
`198.51.100.254`. Each slot occupies its own /30 with no
overlap with neighbours.

64 is well above realistic parallelism for these scripts. The
Go pool sized for 127 because its paired-/32 layout wastes only
one address per pair; the /30 layout costs two extra addresses
per slot (network and broadcast) but gains the connected-route
simplicity, which is the better trade. Widening to a second /24
(TEST-NET-1) for 128 slots doubles the address management code
for no realistic gain at the corpus's scale.

## 7. Cross-process coordination via flock(2)

The pool lives at `/run/bpfman-net-pool/` and is global per
host. All bpfman-shell processes on the same machine coordinate
through the same pool directory: interactive sessions, GNU
parallel runners, and ad-hoc debugging coexist safely because
they share the flock arbitration over the same set of
lockfiles. Each slot has its own lockfile (`01.lock` through
`64.lock`). Acquiring a slot means opening the lockfile and
taking `flock(LOCK_EX|LOCK_NB)`. The kernel releases the flock
when the holding process exits, including under `kill -9`, so
the pool is self-cleaning against crashes without a daemon or a
cleanup script.

Each lockfile's body is a small JSON document carrying
provenance: `test_name`, `ns_name`, `link_a_name`,
`acquired_at`, and `released_at`. `acquired_at` is written
immediately after the lock is taken; `released_at` is written
just before the lock is released. On a clean exit both fields
are populated. On `kill -9` the body retains the acquire-time
fields with no `released_at`, and the next acquirer's
stale-state check (see section 9) attributes the leak to the
right test.

## 8. Acquire algorithm

Enumerate slots `[1, 64]`. For each slot, compute a sort key:
zero time (oldest possible) if no lockfile exists yet;
`released_at` from the body if present; file mtime otherwise.
Sort ascending. Unused slots win first; legacy and
crash-leaked slots fall back to mtime; cleanly released slots
sort by their explicit timestamp.

Walk the sorted candidates. For each, open the lockfile with
`O_RDWR|O_CREATE` mode `0600`, then try `flock(LOCK_EX|LOCK_NB)`.
If the lock succeeds, re-read the body (state may have advanced
since the sort scan), run `assertSlotClean` against the previous
provenance, write a fresh acquire-time body, and return the
slot index plus the open file. If `flock` fails, close and move
on. If no slot can be acquired, error with "more than 64
concurrent pairs in flight".

`released_at` is the primary sort key by design. mtime is a
side effect of writing; if a process writes acquire-time
provenance, its mtime now points at "newly acquired", which is
the opposite of the FIFO intent. Sorting by `released_at` makes
cooldown an explicit invariant rather than an accidental one.
mtime stays as the fallback for files that exist but predate
the schema, or that were created by a process that died before
writing `released_at`.

The re-read after locking is necessary because between the sort
scan and the lock acquisition another process may have
released, re-acquired, and re-released the slot. The body we
sorted against is potentially stale; the body we validate
against must be whatever is in the file under the lock we now
hold.

## 9. Stale-state detection

`assertSlotClean` runs immediately after the lock acquisition
and the body re-read. It validates two facts. If
`prev.link_a_name` is non-empty, it looks up an interface of
that name in the host netns; if present, the call fails. If
`prev.ns_name` is non-empty, it looks up a netns of that name;
if present, the call fails.

Both checks use targeted lookups (not link-table dumps) to
avoid `NLM_F_DUMP_INTR` under parallel churn and to answer the
precise question "did the previous tenant clean up?" rather
than the broader "what is in the link table?".

A failure aborts the script with full attribution: which test
previously held the slot, when it released (or that it never
did), and which resource is still present. The next test fails
as a canary, surfacing the leak loud and attributable rather
than letting it propagate into mystery EEXIST or EBUSY further
down the line.

## 10. NetPair changes

Two new fields, both unexported. `slot uint32` is the pool
index this handle leased, or zero when the caller supplied
explicit addresses. `lockFile *os.File` holds the open
flock'd file backing the slot lease, or nil in explicit-address
mode. Both are released by `net release` as part of teardown.

The public observable surface stays the five identity strings
the DSL exposes through `$pair.field`. The pool plumbing is
package-private; access from `cmd/bpfman-shell/net.go` goes
through package-local accessor methods or a combined
`ReleaseSlotResources` helper that handles the
provenance-write-and-close sequence atomically.

## 11. Release sequence

The auto-mode release runs in this order on the first call.
First tear down topology: `ip link del HostLink` (the kernel
reaps PeerLink with it), then `ip netns del Ns`, both ignoring
errors because missing resources are the desired terminal
state. Then write provenance plus `released_at` to the
lockfile body. Then close the lockfile, which is when the
kernel releases the flock. Finally set `pair.Released = true`.

Subsequent calls short-circuit at the `Released` latch. The
teardown happens before the provenance write so the body
records the canonical "what was released" payload. The flock
release happens before the `Released` flip so the slot becomes
externally available before subsequent operations on the local
handle start short-circuiting. `Released` is a process-local
guard only; cross-process visibility comes entirely from the
flock lifetime and the provenance file. Once `Released` is
set, no further work happens against this handle.

Explicit mode skips the provenance write and the lockfile
close entirely: no slot, no flock to release.

## 12. Scope boundary

In scope for v1: IPv4 only (consistent with
`PLAN-net-builtin.md` section 9); 64 slots over a single /24;
pool root at `/run/bpfman-net-pool/` (root-only tests; matches
`/run/bpfman` siblinghood); stale-state detection on link and
netns names only.

Out of scope: deterministic MAC addresses (the Go pool
mentions them; the shell corpus has never asked); IPv6
(deferred per the parent plan); configurable pool root via
flag (add when a second use case appears); bandwidth-limit or
queueing knobs per slot; multiple veth pairs in one netns
(out of scope per the parent plan).

If a future test wants any of these, the discussion is about
extending the `net` surface, not about pool internals. The
pool mechanism is the same regardless of what the slot covers.

## 13. Migration plan

Two commits.

First, the mechanism and policy: add the pool coordinator, the
/30 auto path, the explicit-address-only path (no route
management), and the NetPair changes. Drop `--no-routes`. Unit
tests cover both modes plus pool ordering, exhaustion, and
stale-state attribution via a per-test `t.TempDir()` standing
in for `/run/bpfman-net-pool/`.

The commit message records that the migrated corpus scripts in
`e2e/new/` run-time-break briefly until the second commit
lands: they pass `--host-addr` and `--peer-addr` to an
explicit mode that no longer manages routes. The break window
is bounded by how quickly the second commit follows. The
break does not affect `make test` (the scripts are not in its
path) so HEAD's CI stays green throughout.

Second, the corpus sweep: drop `--host-addr` and `--peer-addr`
from the nine `TestMultiProg{TC,TCX,XDP}_*.bpfman` scripts.
Same diff pattern as the original net migration. Expected
roughly 27 lines removed across the corpus (three lines per
script).

The checker-tightening commit deferred in
`PLAN-net-builtin.md` section 11 is unchanged by this work;
folding it in is a separate v2 effort.

## 14. Alternatives considered

### Static per-script address partitioning

Each script hardcodes a different /32 pair (or /30) with no
pool. Rejected because parallelism becomes a per-script
concern: adding a tenth script requires careful coordination
on which addresses are free, and concurrent runs of the same
script collide. The pool is the right level of abstraction; a
static partition only defers the problem.

### Launcher-side allocation

GNU parallel (or any other runner) sets `BPFMAN_NET_SLOT=N`;
the script reads N and builds addresses from it. Rejected
because it places the parallel-safety contract on the caller.
`sudo bpfman-shell script.bpfman` without the env var would
fall back to default behaviour, and the "default" then has to
be either broken (collisions) or arbitrary (always slot 1).
The pool inside `net` makes parallel-safe the default and
forms a closure: scripts run safely under any launcher,
including no launcher.

### Daemon-based slot allocation

A long-running `bpfman-net-pool` daemon hands out slots over a
Unix socket. Rejected as over-engineered; flock(2) gives
equivalent cross-process semantics with zero infrastructure
and self-cleaning crash behaviour. The kernel is already a
slot arbiter through flock; an in-userspace one would be a
second copy of the same machinery.

### Asymmetric mode semantics

Auto mode uses /30, explicit mode keeps /32 plus manual
routes. Tempting because it preserves the migrated corpus's
current behaviour. Rejected because the asymmetry is harder
to explain than picking one shape and applying it everywhere.
The explicit-mode contract becomes "set addresses, do nothing
else", which is honest about what the API offers. Callers
that need routes can always reach for `ip route`; the escape
hatch from `PLAN-net-builtin.md` section 4 covers exactly
this case.

### Removing explicit mode entirely

The corpus never uses it post-sweep, so why keep it? Rejected
for v1 because a future test with a legitimate need for a
specific address (matching an external router's expectation,
overlapping with an established VPN's CIDR, testing edge
cases of address handling) is plausible enough to keep the
escape hatch. Removing it later if it stays unused for
several quarters is a one-line API simplification; adding it
back later is a breaking change with version-skew concerns.
