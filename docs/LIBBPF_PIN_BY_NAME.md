# LIBBPF_PIN_BY_NAME and Map Sharing Between Go and Rust bpfman

This document records the findings from investigating why Rust bpfman
and Go bpfman produce different map-read results in the TC operator
integration tests (TestTcGoCounterLinkPriority).

## The Symptom

The Rust operator test reads 6601 rx_packets from a tc_counter program
that has been moved to position 1 in a TC dispatcher chain. The Go
operator test reads 0 rx_packets from the same scenario.

Both implementations produce identical dispatcher chain_call_actions
bitmasks. The chain semantics are correct in both. The difference is
in map sharing behaviour, not dispatcher logic.

## TC Dispatcher Chain Semantics (Verified Identical)

Both Go and Rust compute the same chain_call_actions for the default
TC proceed-on of {Pipe, DispatcherReturn}:

    raw actions:       Pipe=3, DispatcherReturn=30
    computed mask:     (1 << 3) | (1 << 30) = 0x40000008
    chain_call_shift:  1 (TC shifts by 1 to accommodate TC_ACT_UNSPEC=-1)
    shifted mask:      0x40000008 << 1 = 0x80000010

The BPF dispatcher checks: `(1U << (ret + 1)) & chain_call_actions[i]`

For TC_ACT_OK (ret=0): `(1 << 1) & 0x80000010 = 0x2 & 0x80000010 = 0`

The chain stops. A program at position 0 returning TC_ACT_OK does not
pass packets to position 1. This is correct and intentional: TC_ACT_OK
is excluded from the default proceed-on set to match Rust bpfman.

## The Root Cause: LIBBPF_PIN_BY_NAME

### What tc_counter_pinned.bpf.o declares

The tc_counter BPF program (used by the operator tests) declares its
map with the `LIBBPF_PIN_BY_NAME` pinning flag:

```c
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct tc_stats);
    __uint(pinning, LIBBPF_PIN_BY_NAME);   // <-- this flag
} tc_stats_map SEC(".maps");
```

This flag tells BPF loaders: "pin this map by its name so that
separately loaded programs sharing the same map name can reuse the
same kernel map object."

### How Each Loader Library Handles It

#### aya (Rust bpfman's loader)

aya respects `LIBBPF_PIN_BY_NAME`. When the flag is set and no
explicit `map_pin_path()` is configured on the `EbpfLoader`, aya
defaults to pinning at `/sys/fs/bpf/{map_name}`.

Rust bpfman creates its `EbpfLoader` without calling `map_pin_path()`:

```rust
let mut loader = EbpfLoader::new();
loader.allow_unsupported_maps();
// no map_pin_path() call
let mut ebpf = loader.load(&program_bytes)?;
```

Consequence: the first load of tc_counter pins `tc_stats_map` to
`/sys/fs/bpf/tc_stats_map`. The second load finds this existing pin
and reuses the same kernel map. Both program instances share one map.

Verified via bpftool: both program loads report the same map_ids
(e.g., 219547) despite being separate program instances.

The aya CHANGELOG documents this behaviour:

> fix libbpf_pin_by_name -- Aligns with libbpf for the special
> LIBBPF_PIN_BY_NAME map flag. Specifically if the flag is provided
> without a pin path default to '/sys/fs/bpf'.

The aya integration test confirms it:

```rust
let map_pin = Path::new("/sys/fs/bpf/map_pin_by_name");
assert!(&map_pin.exists());
```

#### cilium/ebpf (Go bpfman's loader)

cilium/ebpf also supports `LIBBPF_PIN_BY_NAME`. It parses the
pinning field from ELF BTF and stores it in `MapSpec.Pinning`:

```go
// types.go
type PinType uint32

const (
    PinNone   PinType = iota  // 0
    PinByName                  // 1 - mirrors enum libbpf_pin_type
)
```

However, cilium/ebpf requires an explicit `MapOptions.PinPath` to
activate pin-by-name behaviour. Unlike aya, it does NOT default to
`/sys/fs/bpf/`. If `PinPath` is empty and `Pinning` is `PinByName`,
`NewMapWithOptions` returns an error:

```go
case PinByName:
    if opts.PinPath == "" {
        return nil, fmt.Errorf("pin by name: missing MapOptions.PinPath")
    }
    path := filepath.Join(opts.PinPath, spec.Name)
    m, err := LoadPinnedMap(path, &opts.LoadPinOptions)
    if errors.Is(err, unix.ENOENT) {
        break  // doesn't exist yet, create it
    }
    // exists: check compatibility and reuse
```

Go bpfman explicitly clears the pinning flag before loading:

```go
// platform/ebpf/load.go:53
mapSpec.Pinning = ebpf.PinNone
```

This ensures each program load gets its own independent map. Maps are
then pinned manually to per-program directories after loading, purely
for persistence (not for sharing).

### Summary Table

| Aspect                     | aya (Rust)                        | cilium/ebpf (Go)                |
|----------------------------|-----------------------------------|---------------------------------|
| Parses LIBBPF_PIN_BY_NAME | Yes                               | Yes                             |
| Default pin path           | `/sys/fs/bpf/` (if no explicit)  | None (error if not provided)    |
| Auto map reuse             | Yes, implicit                     | Only with explicit PinPath      |
| Go bpfman override         | N/A                               | `mapSpec.Pinning = PinNone`     |
| Separate loads share maps  | Yes (same map name = same map)   | No (each load = own map)        |

## Impact on Operator Tests

### Rust: TestTcGoCounterLinkPriority shows 6601 packets

1. Operator loads tc_counter twice (or more) via Rust bpfman
2. aya pins `tc_stats_map` to `/sys/fs/bpf/tc_stats_map` on first load
3. Subsequent loads reuse the same kernel map
4. The copy at position 0 writes rx_packets into the shared map
5. The counter daemon reads the shared map and sees accumulated counts
6. It does not matter which position the "original" program occupies;
   all copies write to the same map

### Go: TestTcGoCounterLinkPriority shows 0 packets

1. Operator loads tc_counter twice (or more) via Go bpfman
2. Each load creates its own independent `tc_stats_map`
3. The original program is at position 1 (lower priority)
4. The chain stops at position 0 (TC_ACT_OK not in proceed-on)
5. The original's private map stops accumulating
6. The counter daemon reads the original's frozen map and sees 0 growth

## Verified Experimentally

### Go bpfman (integration-tests/test-tc-slow-map-reads.sh)

Two separately loaded programs with independent maps:

```
PHASE1-3s: prog_A rx_packets=38  prog_B rx_packets=0
PHASE1-6s: prog_A rx_packets=51  prog_B rx_packets=0
  (attach B at position 0, A moves to position 1)
PHASE2-3s: prog_A rx_packets=52  prog_B rx_packets=14   # A frozen
PHASE2-6s: prog_A rx_packets=52  prog_B rx_packets=16   # A frozen
PHASE2-9s: prog_A rx_packets=52  prog_B rx_packets=21   # A frozen
```

A's count froze at 52 when it moved to position 1. B's count grew at
position 0. Each program has its own map; isolation is correct.

### Rust bpfman (integration-tests/test-tc-slow-map-reads-rust.sh)

Two separately loaded programs with shared maps:

```
PHASE1-3s: prog_A rx_packets=54  prog_B rx_packets=54
PHASE1-6s: prog_A rx_packets=63  prog_B rx_packets=64
  (attach B at position 0, A moves to position 1)
PHASE2-3s: prog_A rx_packets=80  prog_B rx_packets=80
PHASE2-6s: prog_A rx_packets=86  prog_B rx_packets=86
PHASE2-9s: prog_A rx_packets=91  prog_B rx_packets=91
```

Both programs report identical counts throughout because they share
the same kernel map via `/sys/fs/bpf/tc_stats_map`. The counter never
appears to freeze because whichever copy is at position 0 writes to
the shared map.

Verified via bpftool:
- Go: prog_A map_ids=219602, prog_B map_ids=219608 (different maps)
- Rust: prog_A map_ids=219547, prog_B map_ids=219547 (same map)

## Debug Logging Added

To trace these findings, debug logging was added to:

- `manager/attach_tc.go`: logs raw_actions, computed_mask,
  chain_call_shift, and shifted_mask when computing proceed-on
- `manager/executor_dispatcher.go`: logs per-slot position,
  program_id, program_name, priority, link_id, proceed_on_raw,
  chain_call_shift, and chain_call_actions during dispatcher rebuild

Example output:

```
attachTC proceed-on program_id=182455 raw_actions="[3 30]"
    computed_mask=0x40000008 chain_call_shift=1 shifted_mask=0x80000010

TC dispatcher slot config position=0 program_id=182455
    program_name=stats priority=0 link_id=339784
    proceed_on_raw=0x40000008 chain_call_shift=1
    chain_call_actions=0x80000010
```

## Conclusions

1. Go bpfman's TC dispatcher logic is correct. The chain stops at
   position 0 when TC_ACT_OK is not in proceed-on.

2. The Rust operator test passes as a side effect of aya's implicit
   map sharing, not because position 1 receives packets. It is
   testing map accumulation across shared instances, not chain
   propagation.

3. Go bpfman's map isolation (PinNone) is arguably more correct from
   a program isolation standpoint. Each loaded program gets its own
   maps unless explicit sharing is requested via MapOwnerID.

4. The operator test itself is conflating "can we read the map" with
   "is the program receiving packets." With shared maps, these are
   not equivalent: any copy at any position writes to the shared map.

5. cilium/ebpf fully supports PinByName but requires explicit
   opt-in via MapOptions.PinPath. Go bpfman additionally clears the
   flag to PinNone, ensuring isolation by default.
