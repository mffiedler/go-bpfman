# Rust parity issues

Issues found while proving go-bpfman vs Rust bpfman parity on
2026-06-26. Each is reproducible from the repository root with the
binaries described in `ENVIRONMENT.md`. Go commands take
`--runtime-dir /run/bpfman-parity-go`; all run under `sudo`.

## On the word "divergence"

"Divergence" here means an observed behavioural difference between the
two implementations on the same input -- nothing more. It is not a
verdict on which side is correct. These were not checked against a
written specification, so for any given difference either side could be
at fault, or both could be defensible.

Where one side looks like it has a bug (for example Rust's
non-deterministic equal-priority ordering), the note says "appears to
be a defect in <impl>" and gives the evidence -- but that is a
calibrated suspicion, not a proof. The exception is ISSUE-7
(`--map-owner-id` not sharing the kernel map), where the direction is
settled: the kernel evidence plus the documented purpose of the flag
establish that Rust is broken and Go is correct. Two cautions in
particular:

- A difference does not establish that Go is right and Rust is wrong.
  The Rust side may be buggy; so may the Go side. Go is the
  implementation under test, not the reference spec.
- That Go is deterministic where Rust is not does not prove Rust is
  broken. A consistent implementation may be preferable, but that is a
  subjective judgement and is not being asserted here; the defensible,
  objective claim is only that the Rust behaviour is not reproducible.

## ISSUE-1 (resolved): the XDP test object used a non-conformant section name

The XDP test object originally declared its program with
`SEC("xdp/xdp_stats")`. Go loaded it; Rust rejected it:

```
$ sudo ./bin/bpfman-rs load file --path e2e/testdata/bpf/xdp_counter.bpf.o --programs xdp:xdp_stats
[ERROR bpfman] Error: failed to load programs: error parsing BPF object: invalid program section `xdp/xdp_stats`
```

This is not a Rust defect. Per the kernel libbpf section-name rules,
XDP is not a `type+` family: the only valid `xdp/<extra>` suffixes are
`cpumap` and `devmap`, so `xdp/xdp_stats` is not a valid XDP section.
The `xdp/<name>` form was a libbpf < 0.2 convention from when one ELF
section held one program; modern toolchains key on the function name
and a plain `SEC("xdp")` is correct. Rust rejecting the bad section is
conformant; Go accepting it is leniency in cilium/ebpf, not an extra
capability.

The asymmetry the original note recorded -- Rust accepts `TYPE/<name>`
for kprobe, uprobe, tracepoint, fentry and tc but not XDP -- follows
from the same rule: those are `type+` families where `/<extra>` is the
attach target, whereas XDP is not.

Resolved: the test objects now use the canonical `SEC("xdp")`, so the
same `.o` loads in both Go and Rust.

## ISSUE-2: Rust silently downgrades kretprobe -> kprobe and uretprobe -> uprobe

Go has distinct load-time program types `kretprobe` and `uretprobe`.
Rust's `load --programs` advertises only
`fentry,fexit,kprobe,tc,tcx,tracepoint,uprobe,xdp`. But Rust does not
*reject* `kretprobe:`/`uretprobe:` -- it accepts the token and loads a
plain `kprobe`/`uprobe` (an entry probe), with no warning.

Same command, both implementations:

```
load file kprobe_counter.bpf.o --programs kretprobe:kprobe_counter
```

- Go: `program_type=kretprobe`; after `link attach kprobe`, the link is
  `kind=kretprobe`, `details.retprobe=true` (attaches at function
  return).
- Rust: `Program Type: kprobe`; after `attach kprobe`, the link is a
  plain entry kprobe (no retprobe indicator).

Confirmed at the kernel level, not inferred from the labels. With the
two links attached, `bpftool link show` reports them differently:

```
<go-link-id>:   perf_event  prog <P>   kretprobe ... vfs_read
<rust-link-id>: perf_event  prog <Q>   kprobe    ... vfs_read
```

So Go's attaches at function return and Rust's at function entry -- the
kernel distinguishes them, the difference is real.

`uretprobe:uprobe_counter` behaves the same way: Go -> `uretprobe`
(retprobe=true), Rust -> `uprobe`.

Impact: on the same input the two produce different program types, and
a caller asking Rust for a return probe gets an entry probe with exit
status 0. Rust accepting the `kretprobe:`/`uretprobe:` token but loading
a plain entry probe looks like a defect on the Rust side -- it arguably
should either honour the return-probe type or reject an unknown one --
but I have not confirmed the intended semantics against a spec, so this
is a calibrated suspicion, not a proof that Go's behaviour is the
correct one. Transcripts: `kretprobe-baseline.*`, `uretprobe-baseline.*`.

## ISSUE-3: Rust XDP `--priority` help says `1-1000` but accepts 0

`bpfman-rs attach <id> xdp --help` documents
`[possible values: 1-1000]`, implying `--priority 0` is rejected. It is
not: Rust accepts `--priority 0` and stores priority 0, position 0 --
matching Go. So this is a documentation/help inaccuracy, not a
behavioural divergence. (Recorded because the help text would otherwise
lead you to predict a divergence that does not exist -- prove, do not
assume.)

## ISSUE-4: error exit codes differ for the same failure class

Both implementations correctly fail the same invalid commands, but the
exit codes are not aligned:

| Failure | Go exit | Rust exit |
| --- | --- | --- |
| wrong-kind attach (xdp prog via tc) | 1 | 101 |
| malformed `--programs` value | 80 | 2 |
| missing object file | 1 | 1 |
| `get` non-existent id | 1 | 1 |

The plan asks for matching exit status / failure class. Failure class
matches everywhere (both reject), and final state matches (nothing
left loaded), but exit codes diverge for the wrong-kind and malformed
cases. Rust's `101` is its panic/abort code; `2` is its clap usage
error. Go uses `1` and an `80`-class for argument validation.

## ISSUE-5: Rust `get`/`list` have no JSON output

Go supports `-o json` (and `-o text`) on every verb. Rust `get
program`, `get link`, `list programs`, `list links` are text-only (no
`-o`/`--json`). Structured comparison therefore maps Go JSON fields to
Rust text labels by hand. The plan's "use JSON whenever both CLIs
support it" applies to Go only here.

## ISSUE-6: `--global` hex value format differs (`0x` prefix)

Go accepts both `weight=0x0000000000000005` and bare
`weight=0000000000000005`. Rust accepts only the bare hex form and
rejects the `0x` prefix:

```
$ sudo ./bin/bpfman-rs load file --path tc_exact.bpf.o --programs tc:stats -g weight=0x0000000000000005
error: invalid value 'weight=0x0000000000000005' for '--global <GLOBAL>...': invalid input
```

So a `--global name=0x...` line that works in Go fails in Rust (exit
2). The bare-hex form works in both. Unknown global keys are rejected
by both (parity): Go `global variable "boguskey" not found`, Rust
`symbol with name ... not found`, both exit 1.

## ISSUE-7: `--map-owner-id` does not share the kernel map in Rust

Loading a second program with `--map-owner-id <owner>` should make the
sharer reuse the owner's map. Verified at the kernel level with
`bpftool prog show`:

- Go: owner and sharer programs both report `map_ids 414612` -- the
  same kernel map object. Sharer record `map_owner_id` = owner id.
- Rust: owner reports `map_ids 414579`, sharer reports `map_ids 414583`
  -- distinct kernel map objects. Rust does record the relationship
  (sharer's `Map Owner ID: <owner>`, `Map Pin Path` pointing at the
  owner's pin dir), but the loaded kernel program references a fresh
  map, not the owner's.

Same object (`tcx_counter.bpf.o`), same command shape. The
kernel-visible effect differs: `bpftool prog show` reports the same
`map_ids` on Go's owner and sharer, but distinct `map_ids` on Rust's.
`--map-owner-id` exists to make the sharer reuse the owner's map, and
the loaded Rust programs demonstrably do not share one at the kernel
level, so this is a Rust-side bug: Go behaves as intended; Rust records
the owner relationship but loads a fresh map instead of reusing the
owner's. This is the one issue here whose direction is settled --
established by the kernel evidence plus the documented purpose of the
flag, and consistent with the project's prior finding that Rust map
sharing is broken. (The general calibration note still governs the
other issues, where the correct/broken split is not established.)

## ISSUE-8: equal-priority tie-break order diverges; Rust is non-deterministic

Attaching the same XDP program three times at the same priority (100)
to one interface builds a 3-member dispatcher chain. The order among
the equal-priority members differs between implementations, and Rust's
order is not stable run to run.

Labelling the attaches #1, #2, #3 and reading the resulting chain as
`[pos0, pos1, pos2]`:

- Go: `[#3, #1, #2]` on 4/4 runs -- deterministic.
- Rust: `[#3, #2, #1]` on 3/8 runs and `[#3, #1, #2]` on 5/8 runs --
  non-deterministic.

Both consistently put the most-recently-attached link at position 0;
they disagree on the order of the two older members, and Rust flips
between the two orders across runs.

Impact: with equal priorities the execution order of chained programs
is implementation-defined, and in Rust it is not reproducible run to
run. A chain that relies on equal-priority ordering is unsafe across
implementations, and unstable even within Rust; use distinct priorities
for a defined order.

This does not establish that Go's particular order is the correct one
-- equal-priority ordering may simply be unspecified, in which case any
stable order is acceptable. The narrow, objective claim is only that
Rust's order is not reproducible whereas Go's is stable. Whether stable
is "better" is a reasonable view but a subjective one, not asserted
here. Transcripts: `ordering-tiebreak.*` (single run); determinism
measured over repeated runs.

## ISSUE-9 (minor): TCX rejects a duplicate program with a raw kernel error in Rust

Attaching the same program twice to one TCX hook fails on both (correct
-- native mprog forbids it), but the error quality differs:

- Go: `program N is already attached to <iface> ingress as link M;
  detach it first` (exit 1).
- Rust: `failed to attach tc program ...: bpf_mprog_attach failed ...
  File exists (os error 17)` (exit 1).

Same failure class and exit status; Go gives an actionable bpfman-level
message, Rust surfaces the raw kernel `EEXIST`. Transcripts:
`ordering-tcx.*`.

## ISSUE-10: `--container-pid` is accepted by Go, unsupported in Rust

What I observed (not the deeper namespace semantics, which I did not
verify):

- Go uprobe `--container-pid 1`: exits 0 and the link reports
  `kernel_seen: true`. I did not confirm the probe actually fires inside
  pid 1's namespace -- only that Go accepts the flag and the attach
  succeeds.
- Rust uprobe `--container-pid 1`: fails with `Unable to attach uprobe
  in container with pid 1` (the help marks it "NOT CURRENTLY
  SUPPORTED").
- Rust kprobe `--container-pid 1`: fails with `kprobe container option
  not supported yet`.

So `--container-pid` is a Go-only capability at the CLI: Go takes it and
succeeds, Rust rejects it. Container uprobe parity is not achievable
against this Rust build regardless of whether Go's implementation is
fully correct.

## Non-issue notes (verified, no action)

- Daemonless: both CLIs manage state directly; no `bpfman-rpc` daemon
  was needed. Rust state in `/run/bpfman`, Go in the isolated runtime
  dir.
- Idempotency matches: second detach / second unload both exit 1 on
  both implementations; first exits 0.
- Metadata round-trips on both (`owner=parity` visible in get/list).
- `--metadata` and `--application` accepted at load by both, and each
  stores the application name as a metadata key, but the key itself
  differs: Go uses `bpfman.io/application`, Rust uses
  `bpfman_application`. Metadata round-trips within each implementation
  (get, list, and the `--application` list filter all use the same key
  they wrote), so neither is broken; the two simply do not share the
  key. Go keeps its Kubernetes-label-style namespaced key deliberately.
