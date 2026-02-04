# TC Dispatcher Implementation Plan

This document describes what is needed to implement TC dispatcher support,
building on the existing XDP dispatcher infrastructure.

## Current State

### What Exists

| Component | Status | Location |
|-----------|--------|----------|
| TC dispatcher BPF program | Done | `dispatchers/tc_dispatcher.bpf.c` |
| TC dispatcher bytecode | Done | `bpfman/dispatcher/tc_dispatcher.bpf.o` |
| Dispatcher path utilities | Done | `bpfman/dispatcher/paths.go` |
| DispatcherState type | Done | `bpfman/managed/dispatcher.go` |
| Dispatcher store (SQLite) | Done | `interpreter/store/sqlite/` |
| Dispatcher cleanup actions | Done | `bpfman/action/action.go` |
| Cleanup on detach | Done | `bpfman/manager/manager.go` |
| TCDetails type | Done | `bpfman/managed/link_details.go` |
| TC link storage | Done | `tc_link_details` table |

### What Needs Implementation

| Component | Description |
|-----------|-------------|
| `AttachTC` manager method | Orchestrates TC attachment via dispatcher |
| `createTCDispatcher` helper | Creates dispatcher for tc-ingress or tc-egress |
| `AttachTCDispatcher` kernel op | Loads TC dispatcher, attaches to clsact qdisc |
| `AttachTCExtension` kernel op | Attaches extension to TC dispatcher slot |
| CLI `attach tc` command | User interface for TC attachment |

## TC vs XDP Differences

### Qdisc Setup

XDP attaches directly to the network interface. TC requires a qdisc
(queueing discipline) to be present first:

```bash
# XDP - direct attachment
ip link set dev lo xdpgeneric obj prog.o sec xdp

# TC - requires clsact qdisc
tc qdisc add dev lo clsact
tc filter add dev lo ingress bpf obj prog.o sec tc
```

The clsact qdisc is a special classifier-action qdisc that provides
both ingress and egress hooks without packet scheduling.

### Two Dispatchers Per Interface

XDP has one hook point (ingress only). TC has two:

| Dispatcher Type | Hook | Direction |
|-----------------|------|-----------|
| `tc-ingress` | TC ingress | Packets entering the interface |
| `tc-egress` | TC egress | Packets leaving the interface |

This means a single interface can have up to 20 TC programs (10 ingress
+ 10 egress) plus 10 XDP programs.

### Return Values

XDP and TC use different return value conventions:

| XDP Return | Value | TC Return | Value |
|------------|-------|-----------|-------|
| XDP_ABORTED | 0 | TC_ACT_UNSPEC | -1 |
| XDP_DROP | 1 | TC_ACT_OK | 0 |
| XDP_PASS | 2 | TC_ACT_SHOT | 2 |
| XDP_TX | 3 | TC_ACT_PIPE | 3 |
| XDP_REDIRECT | 4 | TC_ACT_REDIRECT | 7 |

The TC dispatcher's proceed-on mask uses TC return values, not XDP values.

### Program Type

TC programs use `BPF_PROG_TYPE_SCHED_CLS` (scheduler classifier) rather
than `BPF_PROG_TYPE_XDP`.

## Implementation Details

### 1. Kernel Operations Interface

Add to `interpreter/interfaces.go`:

```go
// TCDispatcherResult holds the result of loading a TC dispatcher.
type TCDispatcherResult struct {
    DispatcherID  uint32 // Kernel program ID of the dispatcher
    LinkID        uint32 // Kernel link ID (TC link)
    DispatcherPin string // Pin path for dispatcher program
    LinkPin       string // Pin path for link
}

// In DispatcherAttacher interface:
type DispatcherAttacher interface {
    // ... existing XDP methods ...

    // AttachTCDispatcher loads and attaches a TC dispatcher to an interface.
    // direction is "ingress" or "egress".
    AttachTCDispatcher(ifindex int, direction string, progPinPath, linkPinPath string,
        numProgs int, proceedOn uint32) (*TCDispatcherResult, error)

    // AttachTCExtension loads a program as Extension type and attaches
    // it to a TC dispatcher slot.
    AttachTCExtension(dispatcherPinPath, objectPath, programName string,
        position int, linkPinPath string) (bpfman.Link, error)
}
```

### 2. Kernel Implementation

Add to `interpreter/ebpf/ebpf.go`:

```go
func (k *Kernel) AttachTCDispatcher(ifindex int, direction string,
    progPinPath, linkPinPath string, numProgs int, proceedOn uint32,
) (*interpreter.TCDispatcherResult, error) {
    // 1. Ensure clsact qdisc exists
    if err := k.ensureClsactQdisc(ifindex); err != nil {
        return nil, fmt.Errorf("ensure clsact qdisc: %w", err)
    }

    // 2. Load TC dispatcher from embedded bytecode
    spec, err := ebpf.LoadCollectionSpecFromReader(
        bytes.NewReader(dispatcher.TCDispatcherBytecode))
    if err != nil {
        return nil, fmt.Errorf("load tc dispatcher spec: %w", err)
    }

    // 3. Inject configuration into .rodata
    // ... similar to XDP dispatcher ...

    // 4. Load collection
    coll, err := ebpf.NewCollectionWithOptions(spec, opts)
    if err != nil {
        return nil, fmt.Errorf("load tc dispatcher: %w", err)
    }

    // 5. Pin dispatcher program
    prog := coll.Programs["tc_dispatcher"]
    if err := prog.Pin(progPinPath); err != nil {
        return nil, fmt.Errorf("pin tc dispatcher: %w", err)
    }

    // 6. Attach to TC hook
    var attachType ebpf.AttachType
    if direction == "ingress" {
        attachType = ebpf.AttachTCXIngress
    } else {
        attachType = ebpf.AttachTCXEgress
    }

    lnk, err := link.AttachTCX(link.TCXOptions{
        Interface: ifindex,
        Program:   prog,
        Attach:    attachType,
    })
    if err != nil {
        return nil, fmt.Errorf("attach tc dispatcher: %w", err)
    }

    // 7. Pin link
    if err := lnk.Pin(linkPinPath); err != nil {
        return nil, fmt.Errorf("pin tc link: %w", err)
    }

    return &interpreter.TCDispatcherResult{
        DispatcherID:  prog.FD(),
        LinkID:        uint32(lnk.FD()),
        DispatcherPin: progPinPath,
        LinkPin:       linkPinPath,
    }, nil
}

func (k *Kernel) ensureClsactQdisc(ifindex int) error {
    // Use netlink to check/create clsact qdisc
    // The cilium/ebpf library may handle this automatically with TCX
    // but we should verify
}
```

### 3. Manager Method

Add to `bpfman/manager/manager.go`. Note that all store operations go
through the executor using reified effects (actions), following the
FETCH/COMPUTE/EXECUTE pattern:

```go
// AttachTC attaches a TC program to a network interface using the
// dispatcher model for multi-program chaining.
//
// direction must be "ingress" or "egress".
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
func (m *Manager) AttachTC(ctx context.Context, programKernelID uint32,
    ifindex int, ifname, direction, linkPinPath string,
) (managed.LinkSummary, error) {
    // FETCH: Get program metadata
    prog, err := m.store.Get(ctx, programKernelID)
    if err != nil {
        return managed.LinkSummary{}, fmt.Errorf("get program %d: %w", programKernelID, err)
    }

    // FETCH: Get network namespace ID
    nsid, err := netns.GetCurrentNsid()
    if err != nil {
        return managed.LinkSummary{}, fmt.Errorf("get current nsid: %w", err)
    }

    // COMPUTE: Determine dispatcher type from direction
    var dispType dispatcher.DispatcherType
    switch direction {
    case "ingress":
        dispType = dispatcher.DispatcherTypeTCIngress
    case "egress":
        dispType = dispatcher.DispatcherTypeTCEgress
    default:
        return managed.LinkSummary{}, fmt.Errorf("invalid direction: %s", direction)
    }

    // FETCH: Look up existing dispatcher or create new one
    dispState, err := m.store.GetDispatcher(ctx, string(dispType), nsid, uint32(ifindex))
    if errors.Is(err, store.ErrNotFound) {
        // KERNEL I/O + EXECUTE: Create new dispatcher
        dispState, err = m.createTCDispatcher(ctx, nsid, uint32(ifindex), direction)
        if err != nil {
            return managed.LinkSummary{}, fmt.Errorf("create TC dispatcher: %w", err)
        }
    } else if err != nil {
        return managed.LinkSummary{}, fmt.Errorf("get dispatcher: %w", err)
    }

    // KERNEL I/O: Attach extension (returns IDs)
    // ... attach TC extension ...

    // COMPUTE: Build save actions from kernel result
    saveActions := computeAttachTCActions(...)

    // EXECUTE: Save through executor
    if err := m.executor.ExecuteAll(ctx, saveActions); err != nil {
        // ... handle error ...
    }

    return summary, nil
}

func (m *Manager) createTCDispatcher(ctx context.Context, nsid uint64,
    ifindex uint32, direction string,
) (managed.DispatcherState, error) {
    // COMPUTE: Determine dispatcher type and paths
    var dispType dispatcher.DispatcherType
    switch direction {
    case "ingress":
        dispType = dispatcher.DispatcherTypeTCIngress
    case "egress":
        dispType = dispatcher.DispatcherTypeTCEgress
    }

    revision := uint32(1)
    linkPinPath := dispatcher.DispatcherLinkPath(dispType, nsid, ifindex)
    revisionDir := dispatcher.DispatcherRevisionDir(dispType, nsid, ifindex, revision)
    progPinPath := dispatcher.DispatcherProgPath(revisionDir)

    // KERNEL I/O: Create dispatcher (returns IDs)
    result, err := m.kernel.AttachTCDispatcher(
        int(ifindex),
        direction,
        progPinPath,
        linkPinPath,
        dispatcher.MaxPrograms,
        tcProceedOnOK, // TC_ACT_OK = 0
    )
    if err != nil {
        return managed.DispatcherState{}, err
    }

    // COMPUTE: Build save action from kernel result
    state := computeDispatcherState(dispType, nsid, ifindex, revision, result, progPinPath, linkPinPath)
    saveAction := action.SaveDispatcher{State: state}

    // EXECUTE: Save through executor
    if err := m.executor.Execute(ctx, saveAction); err != nil {
        return managed.DispatcherState{}, fmt.Errorf("save dispatcher: %w", err)
    }

    return state, nil
}
```

### 4. CLI Command

Add to `cmd/bpfman/cli/attach.go`:

```go
type TCCmd struct {
    ProgramID   ProgramID `arg:"" required:"" help:"Program kernel ID to attach."`
    Interface   string    `arg:"" required:"" help:"Network interface name."`
    Direction   string    `required:"" enum:"ingress,egress" help:"Traffic direction."`
    LinkPinPath string    `help:"Custom link pin path (optional)."`
}

func (c *TCCmd) Run(cli *CLI) error {
    // ... similar to XDPCmd but calls mgr.AttachTC ...
}
```

### 5. TC Dispatcher Config

The TC dispatcher uses a similar config structure to XDP:

```c
struct tc_dispatcher_conf {
    __u8 magic;
    __u8 dispatcher_version;
    __u8 num_progs_enabled;
    __u8 _pad;
    __u32 chain_call_actions[10];
    __u32 run_prios[10];
};
```

The `chain_call_actions` field uses TC return values as the bitmask.

## Testing Plan

### Unit Tests

1. Test `extractDispatcherKey` with TCDetails (already works)
2. Test `computeDispatcherCleanupActions` with TC dispatcher state
3. Test path generation for tc-ingress and tc-egress types

### Integration Tests

Extend `integration-tests/test-dispatcher-cleanup.sh` or create
`test-tc-dispatcher-cleanup.sh`:

```bash
# Load TC program
bpfman load image --program=tc:classifier quay.io/bpfman-bytecode/tc_pass:latest

# Attach ingress
bpfman attach tc --program-id=<id> --direction=ingress lo

# Verify dispatcher exists
ls /sys/fs/bpf/bpfman/tc-ingress/
sqlite3 /run/bpfman/state.db "SELECT * FROM dispatchers WHERE type='tc-ingress'"

# Attach egress (separate dispatcher)
bpfman attach tc --program-id=<id> --direction=egress lo

# Verify both dispatchers
sqlite3 /run/bpfman/state.db "SELECT type, num_extensions FROM dispatchers"

# Detach and verify cleanup
bpfman detach <link-uuid>
```

## Path Examples

For TC ingress on eth0 (ifindex=2) in root namespace:

```
/sys/fs/bpf/bpfman/tc-ingress/dispatcher_4026531840_2_link
/sys/fs/bpf/bpfman/tc-ingress/dispatcher_4026531840_2_1/dispatcher
/sys/fs/bpf/bpfman/tc-ingress/dispatcher_4026531840_2_1/link_0
```

For TC egress on the same interface:

```
/sys/fs/bpf/bpfman/tc-egress/dispatcher_4026531840_2_link
/sys/fs/bpf/bpfman/tc-egress/dispatcher_4026531840_2_1/dispatcher
/sys/fs/bpf/bpfman/tc-egress/dispatcher_4026531840_2_1/link_0
```

## Cleanup Behaviour

The existing cleanup infrastructure handles TC dispatchers automatically
using the FETCH/COMPUTE/EXECUTE pattern:

1. `Detach` fetches the link details and dispatcher state
2. `extractDispatcherKey` recognises `TCDetails` and returns the correct
   dispatcher type based on direction (ingress or egress)
3. `computeDetachActions` builds the complete action sequence, calling
   `computeDispatcherCleanupActions` internally to generate `RemovePin`
   and `DeleteDispatcher` actions when the dispatcher has no remaining
   extensions
4. The executor runs all actions (detach link, delete link, dispatcher
   cleanup)

No changes needed to the cleanup code path - TC dispatchers are handled
by the same generic infrastructure as XDP dispatchers.

## Dependencies

### cilium/ebpf TCX Support

The implementation assumes cilium/ebpf's `link.AttachTCX` function for
modern TCX attachment. This provides:
- Automatic clsact qdisc creation
- Clean link-based lifecycle management
- Support for multi-attach ordering

If TCX is not available (older kernels), fall back to legacy TC attachment
using netlink directly.

### Kernel Requirements

- TCX requires Linux 6.6+ for full functionality
- Legacy TC BPF works on Linux 4.1+
- The dispatcher approach works with both, but TCX is preferred

## Open Questions

1. **TCX vs Legacy TC**: Should we support both attachment methods or
   require TCX? TCX is cleaner but has higher kernel requirements.

2. **Qdisc Lifecycle**: Should bpfman manage clsact qdisc creation and
   deletion, or assume it exists?

3. **Priority Ordering**: TC traditionally uses priority for ordering.
   How does this interact with dispatcher slot positions?

4. **Shared Dispatcher Bytecode**: Can we use the same dispatcher for
   ingress and egress, or do they need separate programs?
