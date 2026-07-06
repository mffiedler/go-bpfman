# Go-side audit

The parity comparison treats Rust as the reference and asks "does Go
match it?". That under-examines Go: wherever the two agree, Go is
assumed correct. This file is the other half -- Go checked against
**kernel ground truth** (`bpftool`, bpffs), independent of Rust, plus a
deliberate hunt for Go-only oddities.

Method: Go runs against the isolated runtime dir
`--runtime-dir /run/bpfman-parity-go`; the oracle is the kernel
(`bpftool prog/map/link show`) and the bpffs pins under that dir, not
Rust's output and not Go's own reporting.

This is a first pass. It covers the CLI lifecycle only; see the surface
map in `RESULTS.md` for what is still unaudited (gRPC/serve, CSI, image
build, the library API).

## Findings

### PASS: clean teardown leaves no kernel residue

For kprobe and for the XDP dispatcher path, after `detach` + `unload`
the kernel objects and bpffs pins are all gone, verified with
`bpftool`:

- kprobe: loaded kernel program id and map id both absent after unload;
  `fs/prog_<id>` and `fs/maps/<id>` removed.
- xdp: the dispatcher kernel program is present while a member is
  attached, absent after the last `detach`; the loaded program is
  absent after `unload`; no leftover entries under `fs/xdp/`.

Go does not leak kernel programs, maps, dispatchers, or pins on a normal
teardown.

### PASS: liveness reporting re-probes, it does not cache

With a kprobe link attached, removing the link's bpffs pin externally
(`rm` the pin behind Go's back, which frees the kernel link) and then
running `link get` flips both `status.kernel_seen` and
`status.pin_present` from `true` to `false`. `detach` then still
succeeds and clears the orphaned record. So Go reflects real kernel and
pin state at query time rather than trusting its store.

### MINOR: `created_at` is zero-valued on some attach responses

The JSON returned by `link attach` carries `record.created_at` for some
program types but not others:

- xdp attach response: `created_at` is `0001-01-01T00:00:00Z` (zero).
- kprobe attach response: `created_at` is populated.

A subsequent `link get` returns a populated `created_at` for both. So
the field is correct in the store; the attach *response* for the
dispatcher path just does not fill it in. Cosmetic, but an
inconsistency in the command output contract.

### NOTE: link IDs are synthetic, not kernel link IDs

Go assigns link IDs from the store's monotonic AUTOINCREMENT counter
(the first handle is 1), distinct from the kernel link id
(`status.kernel.id`). Within one store they are unique and never reused
after delete, and they are stable across CLI invocations (the store is
on disk; the CLI is daemonless). Not a bug -- bpfman owns its link
namespace, as Rust does with its own (hash-like) ids -- but worth
recording. A fresh store (wipe / new runtime dir) restarts the counter
from 1, so link ids are only meaningful within a single store.

### CORRECTION: the deleted-namespace `kernel_seen` claim is unverified

`RESULTS.md` previously implied Go reports a *stale* `kernel_seen: true`
after `ip netns del`. Trying to confirm this, `bpftool link show id
<kernel_link_id>` did not enumerate that XDP-in-netns dispatcher link
even *before* the deletion, so the probe is inconclusive: I cannot say
whether the kernel link object survives the namespace deletion. Given
the pin-removal result above (Go re-probes correctly), the safe
statement is that this case is **unverified**, not that Go is stale.
`RESULTS.md` has been corrected accordingly.

## Not yet audited (Go-side follow-ups)

- Map *contents* correctness (counters advancing as expected under
  traffic), beyond presence of the map object.
- gRPC `serve` daemon, CSI driver, and `image build` surfaces.
- Verifier / malformed-bytecode load-error handling on the Go side.
- Concurrent access to one store (the `--lock-timeout` path).
- Full state survival across a real process restart under load (the
  daemonless store is exercised incidentally, not stress-tested).
- The link-ID counter's behaviour across store recreation.
