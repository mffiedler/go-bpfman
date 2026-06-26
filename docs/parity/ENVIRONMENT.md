# Parity proof environment

This file records the exact environment and the replayable setup steps
used to generate the parity evidence under `docs/parity/`. The intent
is that you can reproduce every step by hand.

## Host

- Kernel: `6.18.33` (`uname -r`).
- Date generated: 2026-06-26.
- A KIND Kubernetes cluster with bpfman-operator is running on this
  host (`/bpfman --csi-support`, `bpfman-agent`, CSI driver). It owns
  ~312 kernel BPF programs visible to `bpftool prog show`. Those are
  baseline noise: kernel BPF objects are global and ID-addressed, so
  our freshly loaded programs get fresh IDs and do not collide. The
  cluster's bpfman runs inside the kind container with its own `/run`,
  so host-level CLI runs do not touch its state store. Verified: the
  cluster `bpfman` process is in a separate mount namespace
  (`mnt:[4026534242]` vs the host's `mnt:[4026531832]`) and its
  `/run/bpfman` is a distinct inode (`108:824` vs the host's
  `26:2300877`).

## Binaries

- Go: `~/src/github.com/frobware/go-bpfman/bin/go-bpfman` -> `./bpfman`
  (symlink to the `go build` output). Stamped build:
  commit `c392521a`, branch `doc-types`, state `clean`,
  built `2026-06-26T12:08:12Z`. Built with `make STAMP=1 bpfman-build`.
- Rust: `~/src/github.com/frobware/go-bpfman/bin/bpfman-rs` ->
  `~/src/github.com/bpfman/bpfman/worktrees/general/target/debug/bpfman`
  (symlink). Reports `bpfman 0.6.0`. Source revision (worktree HEAD):
  `8e5a9d29`.

Both are daemonless CLIs that manage their own state directly. There
is no active host `bpfman-rpc` socket (`/run/bpfman/sock/` is empty).

## Privileges

All invocations run under `sudo` (passwordless sudo is configured).
Loading/attaching BPF and creating interfaces require root.

## State isolation

- Go runs against an isolated runtime dir to avoid any collision with
  Rust or the cluster:
  `--runtime-dir /run/bpfman-parity-go`.
- Rust has no runtime-dir flag; it uses its default `/run/bpfman`. That
  store's managed program/link lists were empty at the start
  (`bpfman-rs list programs` -> header only).
- `/etc/bpfman/bpfman.toml` only contains `[signing] verify_enabled =
  false`; it does not redirect the runtime dir.

## Interface creation recipe (replayable)

XDP, TC, and TCX attach need a network interface. I use a throwaway
veth pair per case and delete it in cleanup. Recipe (matches what the
Go test harness builtin `net veth-pair` does at the netlink level, but
expressed as plain `ip` so it is copy/pasteable):

```sh
# Create a veth pair; HOST is the attach target, HOST+"p" is the peer.
HOST=bpfmanpar0
sudo ip link del "$HOST" 2>/dev/null || true     # idempotent pre-clean
sudo ip link add "$HOST" type veth peer name "${HOST}p"
sudo ip link set "$HOST" up
sudo ip link set "${HOST}p" up

# ... attach BPF to "$HOST" ...

# Teardown:
sudo ip link del "$HOST"                          # removes both ends
```

Notes:
- Deleting one end of a veth pair removes both ends.
- A veth pair (rather than a dummy interface) is used because XDP
  native-mode attach is better supported on veth than on dummy links.
- Interface names are kept under 15 chars (kernel IFNAMSIZ limit).

## Network namespace recipe (for `--netns` cases)

The `--netns` attach cases put one veth end inside a named network
namespace and attach to it through the namespace path:

```sh
NS=bpfmanns
sudo ip netns add "$NS"
sudo ip link add nsveth0 type veth peer name nsveth0p
sudo ip link set nsveth0p netns "$NS"          # move peer into the ns
sudo ip link set nsveth0 up                     # host end up
sudo ip netns exec "$NS" ip link set nsveth0p up
sudo ip netns exec "$NS" ip link set lo up

# attach to nsveth0p as seen inside $NS:
#   go:   link attach xdp <prog> nsveth0p --netns /var/run/netns/$NS --priority 100
#   rust: attach <prog> xdp --iface nsveth0p --netns /var/run/netns/$NS --priority 100

# teardown (removes the ns and the peer end with it):
sudo ip netns del "$NS"
sudo ip link del nsveth0
```

`/var/run/netns/$NS` is the path both CLIs expect for `--netns`
(`/var/run` is the usual symlink to `/run`).

## Network requirement (image-load case only)

The `image-load` case pulls `quay.io/bpfman-bytecode/xdp_pass:latest`
from quay.io, so it needs outbound registry access; every other case is
offline. Anonymous pull of the public image is sufficient (no auth).

## What gets recorded

- `docs/parity/outputs/<case>.go.out`   raw Go transcript (command + output).
- `docs/parity/outputs/<case>.rust.out` raw Rust transcript.
- `docs/parity/RESULTS.md`               per-case pass/fail summary table.
- `docs/parity/ISSUES.md`                problems found, per program type.
- `docs/parity/help/`                    captured `--help` for both CLIs.
