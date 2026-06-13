# Container uprobe attach design notes

This is a working design note for the Go container-uprobe attach path. It
records the problem exposed while adding `KERNEL_LINK_ID` output and a
container-uprobe e2e script. The intent is to separate the facts we have
proved from the design choices still open.

## Summary

The original Go container-uprobe path could attach while the attaching process
was alive, but it did not durably persist the attachment.

The helper enters the target process's mount namespace, attaches the uprobe,
and returns an owning attachment fd to the parent process over a Unix socket.
The original parent kept that fd in memory. It created no link pin and recorded
no durable kernel link ID for this attach path.

That model works only for the lifetime of the process that holds the fd. For
the normal one-shot CLI path, that process is the `bpfman link attach` command
itself. The original model therefore strongly implied that when the command
exited, the fd closed, the attachment disappeared, and the store row remained as
a phantom managed link record. That followed from the confirmed process and fd
lifetime, but still needed an explicit kernel detach observation.

The e2e script that currently passes does so because `bpfman-shell` attaches
and fires traffic in the same process. It proves the namespace attach can work
while the fd is alive. It does not prove persistence across command
boundaries.

A test or example that runs one `bpfman link attach --container-pid` command
and then checks `bpfman link list` from a second command would not prove
that the managed link works if it only observes `KERNEL_LINK_ID=<none>`. It
would be characterising the broken state: the store row still renders after the
process that held the attachment fd has exited. That is useful evidence for the
bug, but it must not be treated as a successful container-uprobe integration
test.

## Current Go implementation

The manager calls a platform facade:

- `AttachUprobeContainer(...)`

That is the real deep module boundary. Callers do not see namespace entry, fd
passing, sockets, inherited locks, or link lifetime handling.

Behind that boundary, `platform/ebpf.attachUprobeViaHelper` does the work:

- loads the pinned BPF program
- duplicates the program fd
- creates a socketpair
- duplicates the writer-lock fd
- builds `ExtraFiles`
- starts the helper through `internal/bpfman/ns.CommandWithOptions`
- receives an attachment fd with `SCM_RIGHTS`
- wraps the received fd as a Cilium link in the host-namespace parent
- pins the link to bpfman's bpffs
- reads the kernel link ID from the same fd

The child-side helper runs through:

- `internal/bpfman/ns`
- `internal/bpfman/ns/runner`

`internal/bpfman/ns` owns the private transport contract:

- `_BPFMAN_MNT_NS` selects the target mount namespace
- a C constructor calls `setns(CLONE_NEWNS)` before the Go runtime starts
- fd 3 is the inherited BPF program
- fd 4 is the Unix socket used to return the attachment fd
- the inherited writer-lock fd is located through `BPFMAN_WRITER_LOCK_FD`

`internal/bpfman/ns/runner` runs after namespace entry. It reconstructs the
program from fd 3, verifies the inherited writer scope, attaches the uprobe,
and returns the owning attachment fd to the parent.

On modern kernels, Cilium may represent the uprobe attachment as a
`BPF_LINK_TYPE_PERF_EVENT`. In that case the owning lifetime handle is the BPF
link fd, not merely the perf-event fd. Returning the perf-event fd and then
closing the Cilium `Link` makes the attach report success while tearing down
the attachment. The Go helper now returns the BPF link fd when one is exposed,
with a perf-event fd fallback for older ioctl-style links.

## Why the original lifetime was not enough

An fd-held link is only alive while some process keeps the owning fd open.

In a long-lived daemon model, this would mean "alive until daemon exit or
restart". In the CLI model, `bpfman link attach` is a one-shot command. The
original implementation stored the fd in a process-local `linkFds` map, the
command returned, and the process exited. The fd then closed immediately.

So the original one-shot CLI behaviour was effectively:

1. attach reports success
2. a store row is committed
3. the command exits
4. the owning fd closes
5. the kernel attachment disappears
6. the store row remains

That is not a defensible durable managed-link semantic. It is a bug or port
gap, not just an intentional `<none>` display choice.

## Rust implementation comparison

The Rust implementation has a separate `bpfman-ns` binary with an operator
readable command shape:

```text
bpfman-ns uprobe \
  --program-pin-path <PROGRAM_PIN_PATH> \
  --link-pin-path <LINK_PIN_PATH> \
  --offset <OFFSET> \
  --target <TARGET> \
  --container-pid <CONTAINER_PID>
```

It opens the target mount namespace, attaches the uprobe, returns to bpfman's
mount namespace, converts the uprobe link into an owned fd link, and pins it.

The key difference is not the flag shape. It is persistence. Rust pins a link.
The current Go path only returns an fd to the parent process.

The Rust "setns back and pin" sequence does not translate directly to normal
Go code. A Go process cannot safely call `setns(CLONE_NEWNS)` after the runtime
has started because the process is multithreaded. The Go cgo constructor solves
only the "enter before Go runtime startup" half.

## Standalone `bpfman-ns` binary

The parent-side pinning spike removed the reason to keep a standalone
`cmd/bpfman-ns` binary in the Go tree. The durable design does not need a
Rust-shaped helper command that performs attach and pinning by itself.

The private bpfman-ns mode still exists. It is an internal self-reexec protocol
selected by `BPFMAN_MODE=bpfman-ns`, with inherited fds for the program, return
socket, and writer lock. That is the right boundary for Go: the child enters
the target mount namespace and attaches; the host-namespace parent receives the
owning link fd, pins it, and records the kernel link ID.

## Design question

The real decision is:

Do container uprobes remain fd-held, process-lifetime attachments, or do they
become durable pinned links like other managed links?

If they remain fd-held:

- `KERNEL_LINK_ID=<none>` is expected for container uprobes
- the helper must be owned by a long-lived process for the attachment to be
  meaningful
- one-shot CLI attach is not a valid durable operation unless the architecture
  changes to route through a daemon that holds the fd
- a Rust-style operator-facing `bpfman-ns` CLI is misleading
- moving the fd owner to a long-lived daemon would make fd-held links meaningful
  for daemon lifetime, but it would not make them pinned links or survive daemon
  restart

If they become pinned:

- container uprobes should survive the attach command exiting
- the managed link should have a real kernel identity where the kernel exposes
  one
- `KERNEL_LINK_ID=<none>` should mostly disappear for this attach path
- output tests and identity expectations must change
- the received link fd must be pinned into the correct host/bpfman bpffs,
  preferably by the host-namespace parent process

The second option looks like the intended managed-link model. The first option
matches the current implementation but leaves one-shot CLI attach broken.

## Resolved technical problem: host bpffs pinning

The successful spike proved parent-side pinning, not pinning from inside the
target mount namespace.

The child-side helper now returns the owning attachment fd to the parent. On
the tested modern kernel this is a BPF link fd for a
`BPF_LINK_TYPE_PERF_EVENT`. The parent process never left bpfman's mount
namespace, so it wrapped the received fd with Cilium's link API, pinned it into
the host/bpfman bpffs, and read the kernel link ID from the same fd.

Observed spike result:

- the one-shot attach command exited
- `bpftool link list` still showed `123888: perf_event  prog 73928`
- the pin existed at `/run/bpfman/fs/links/73928/e2e_uprobe_call_malloc`
- the link had a real kernel link ID, `123888`

That means the hard cross-namespace bpffs problem disappears for modern kernels.
The parent-side comments that perf-event based uprobes cannot be pinned were
stale: they described the old raw perf-event fd model, not the BPF link fd that
the helper now returns.

There is still a kernel-dependent unsupported case. If the helper only has an
ioctl-style raw perf-event fd, there is no BPF link fd to pin or identify. The
current contract rejects that case explicitly rather than recreating the
one-shot phantom link bug. Supporting it later would require a separate
long-lived fd-owner design; it must not be recorded as a successful durable
managed link.

If a future raw-perf-event fallback is required, the harder problem is pinning
or otherwise owning the attachment from a helper that starts Go execution inside
the target mount namespace.

In the Rust implementation, the helper calls `setns` back to bpfman's mount
namespace before pinning. Normal Go code cannot do that safely after runtime
startup.

Fallback approaches to investigate only if a raw-perf-event case needs support:

- pre-open a host bpffs directory before namespace entry and pin through a
  stable fd path such as `/proc/self/fd/<dirfd>/...`
- extend the C constructor / pre-runtime handoff to preserve the host bpffs
  view in a controlled way
- use another helper process dedicated to pinning in the host namespace

Any approach must be proved on a real container mount namespace. An `unshare -m`
test is not enough because it may still share the host bpffs mount layout.

There is a second durability axis to answer even if host-bpffs pinning works:
does a pinned container-uprobe link survive only bpfman restart, or does it also
survive the target container exiting and its mount namespace/filesystem going
away? The Rust implementation likely has the same constraint, but the Go design
should state which lifetime it can actually guarantee.

## Proposed proof plan

1. Prove the original one-shot CLI bug directly:

   - start a deterministic target process
   - run `bpfman link attach uprobe --container-pid ...` as one command
   - after that command exits, fire traffic from a fresh process
   - read the counter map
   - expected original result: no increment, proving the link detached on exit

2. Turn the proven parent-side pinning spike into production code:

   - attach inside the target mount namespace
   - receive the owning link fd in the host-namespace parent
   - wrap the fd as a Cilium link in the parent
   - pin that link to the usual bpfman link pin path
   - read the kernel link ID from the pinned/received link
   - persist both the bpfman link ID and kernel link ID in the store
   - exit the attaching command
   - fire traffic from a fresh process
   - verify the counter still increments
   - verify `bpfman link list` correlates store, kernel, and pin state

3. If a raw-perf-event fallback is required, prove or disprove host-bpffs
   pinning from the helper:

   - use a real container runtime namespace, not only `unshare -m`
   - attach inside the container mount namespace
   - attempt to pin in the host/bpfman bpffs
   - exit the helper
   - fire traffic from a fresh process
   - verify the counter still increments
   - verify `bpfman link list` correlates store, kernel, and pin state

4. Follow-up design work once durable parent-side pinning is settled:

   - keep the self-reexec helper private unless a separate product need appears
   - keep output and tests expecting real IDs for modern-kernel container
     uprobes

5. If pinning does not work, decide explicitly whether container uprobes require
   a long-lived daemon fd owner. In that design, the one-shot CLI must not claim
   to have created a durable managed link unless it has handed the operation to
   that daemon.

## Non-negotiable invariants

- The helper's kernel mutation must be serialised under the parent operation's
  writer lock.
- The pre-runtime namespace entry ordering must remain explicit and tested.
- The fd returned to any parent fd-holder must be the owning attachment fd.
- Store records must not outlive the actual kernel attachment unless the record
  is explicitly marked as stale or failed. The original fd-held
  container-uprobe path violated this invariant for one-shot CLI attaches.
- Tests must prove traffic after the attach command boundary, not only during
  the same process that attached.
