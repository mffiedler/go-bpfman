# Inspect: State of the Bpfman World

This document describes the inspection/correlation layer that provides a
unified view of bpfman's state across store, kernel, and filesystem. This
abstraction is used by CLI commands and diagnostics.

## Overview

Multiple components need to query bpfman's state:

| Consumer | Purpose |
|----------|---------|
| CLI `list` commands | Display programs, links, dispatchers to users |
| Diagnostic tools | Inspect state for debugging and troubleshooting |

The `inspect` package provides the "state of the bpfman world" by
correlating three sources:

- **Store** — what bpfman believes it manages
- **Kernel** — what's actually alive
- **Filesystem** — what's pinned on bpffs

## Architecture

```
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│  Store          │  │  Kernel         │  │  bpffs.Scanner  │
│  (interpreter)  │  │  (interpreter)  │  │  (leaf package) │
└────────┬────────┘  └────────┬────────┘  └────────┬────────┘
         │                    │                    │
         └────────────────────┼────────────────────┘
                              │
                              ▼
                    ┌─────────────────────┐
                    │  inspect.Snapshot() │
                    │  (correlation)      │
                    └──────────┬──────────┘
                               │
                               ▼
                    ┌─────────────────────┐
                    │  inspect.World      │
                    │  - Programs         │
                    │  - Links            │
                    │  - Dispatchers      │
                    └──────────┬──────────┘
                               │
              ┌────────────────┼────────────────┐
              │                │                │
              ▼                ▼                ▼
        ManagedPrograms()  All Programs   Diagnostics
        (default CLI)      (--all flag)
```

## Package Structure

```
bpffs/
├── bpffs.go         # Mount/unmount operations (existing)
├── scanner.go       # FS layout enumeration (new)
└── scanner_test.go

inspect/
├── inspect.go       # World, Snapshot(), row types
└── inspect_test.go

config/
└── runtime_dirs.go  # ScannerDirs() helper added
```

### Package Roles

| Package | Role |
|---------|------|
| `bpffs` | Leaf primitive: enumerate bpfman's FS layout, parse path conventions |
| `inspect` | Correlation layer: join store + kernel + bpffs into unified World |
| `config` | Runtime directories; provides `ScannerDirs()` for scanner creation |

The `bpffs.Scanner` is a leaf dependency with no knowledge of its consumers.
The `inspect` package imports `bpffs` and performs the correlation.

## Design Principles

### Store-first, `--all` widens

The default view is **what bpfman manages** (store-driven). The `--all` flag
widens to show everything:

- **Managed** — in store (default)
- **Kernel-only** — in kernel but not in store
- **FS-only (orphan)** — on filesystem but not in store or kernel

This keeps "bpfman is about managed programs" crisp while allowing full
visibility when needed.

### Presence as composable flags

Rather than a single `Source` enum, presence is tracked as three independent
booleans:

```go
type Presence struct {
    InStore  bool
    InKernel bool
    InFS     bool
}

func (p Presence) Managed() bool    { return p.InStore }
func (p Presence) KernelOnly() bool { return p.InKernel && !p.InStore }
func (p Presence) OrphanFS() bool   { return p.InFS && !p.InStore && !p.InKernel }
```

This handles overlap naturally (e.g., kernel + FS but not store).

### Narrow interfaces

`Snapshot()` takes minimal interfaces rather than full `interpreter.Store`:

```go
type StoreLister interface {
    List(ctx context.Context) (map[uint32]bpfman.Program, error)
    ListLinks(ctx context.Context) ([]bpfman.LinkSummary, error)
    ListDispatchers(ctx context.Context) ([]dispatcher.State, error)
}

type KernelLister interface {
    Programs(ctx context.Context) iter.Seq2[kernel.Program, error]
    Links(ctx context.Context) iter.Seq2[kernel.Link, error]
}
```

This improves testability and makes dependencies explicit.

## API

### Types

```go
// ProgramRow is a store-first view of a program with presence annotations.
type ProgramRow struct {
    KernelID   uint32
    Name       string   // from store
    Type       string   // from store
    PinPath    string   // from store
    HasPinPath bool
    Presence   Presence
}

// LinkRow is a store-first view of a link with presence annotations.
type LinkRow struct {
    KernelLinkID    uint32
    KernelProgramID uint32
    LinkType        string
    PinPath         string
    HasPinPath      bool
    Synthetic       bool
    Presence        Presence
}

// DispatcherRow is a store-first view of a dispatcher.
type DispatcherRow struct {
    DispType     string
    Nsid         uint64
    Ifindex      uint32
    Revision     uint32
    KernelID     uint32
    LinkID       uint32
    Priority     uint32
    FSLinkCount  int      // count of link_* files (-1 if unknown)
    ProgPresence Presence
    LinkPresence Presence // for XDP link
}

// World is a point-in-time snapshot across all sources.
type World struct {
    Programs    []ProgramRow
    Links       []LinkRow
    Dispatchers []DispatcherRow
    Meta        SnapshotMeta
}

type SnapshotMeta struct {
    Errors []error // non-fatal enumeration errors
}
```

### Entry Point

```go
func Snapshot(
    ctx context.Context,
    store StoreLister,
    kern KernelLister,
    scanner *bpffs.Scanner,
) (*World, error)
```

### Filtered Views

```go
func (w *World) ManagedPrograms() []ProgramRow    // InStore only
func (w *World) ManagedLinks() []LinkRow          // InStore only
func (w *World) ManagedDispatchers() []DispatcherRow
```

For `--all`, use `w.Programs` directly (includes all sources).

## bpffs.Scanner

The scanner is a leaf primitive that enumerates bpfman's filesystem layout.
It parses path conventions and yields typed structures.

### Types

```go
type ScannerDirs struct {
    FS        string // bpffs mount point
    XDP       string // XDP dispatcher directory
    TCIngress string // TC ingress dispatcher directory
    TCEgress  string // TC egress dispatcher directory
    Maps      string // maps directory
    Links     string // links directory
}

type ProgPin struct {
    Path     string
    KernelID uint32
}

type LinkDir struct {
    Path      string
    ProgramID uint32
}

type MapDir struct {
    Path      string
    ProgramID uint32
}

type DispatcherDir struct {
    Path      string
    DispType  string // "xdp", "tc-ingress", "tc-egress"
    Nsid      uint64
    Ifindex   uint32
    Revision  uint32
    LinkCount int    // count of link_* files
}

type DispatcherLinkPin struct {
    Path     string
    DispType string
    Nsid     uint64
    Ifindex  uint32
}
```

### Streaming Iterators

```go
func (s *Scanner) ProgPins(ctx context.Context) iter.Seq2[ProgPin, error]
func (s *Scanner) LinkDirs(ctx context.Context) iter.Seq2[LinkDir, error]
func (s *Scanner) MapDirs(ctx context.Context) iter.Seq2[MapDir, error]
func (s *Scanner) DispatcherDirs(ctx context.Context) iter.Seq2[DispatcherDir, error]
func (s *Scanner) DispatcherLinkPins(ctx context.Context) iter.Seq2[DispatcherLinkPin, error]
func (s *Scanner) PathExists(path string) bool
```

### Malformed Entry Handling

Unparseable entries are skipped and optionally reported via callback:

```go
scanner := bpffs.NewScanner(dirs).WithOnMalformed(func(path string, err error) {
    log.Printf("malformed entry: %s: %v", path, err)
})
```

## Usage Example

### CLI List Command

```go
func listPrograms(ctx context.Context, store interpreter.Store, kernel interpreter.KernelOperations, dirs config.RuntimeDirs, showAll bool) error {
    scanner := bpffs.NewScanner(dirs.ScannerDirs())

    world, err := inspect.Snapshot(ctx, store, kernel, scanner)
    if err != nil {
        return err
    }

    var rows []inspect.ProgramRow
    if showAll {
        rows = world.Programs
    } else {
        rows = world.ManagedPrograms()
    }

    for _, r := range rows {
        fmt.Printf("%d\t%s\t%s\n", r.KernelID, r.Name, r.Type)
        if showAll && !r.Presence.InStore {
            if r.Presence.KernelOnly() {
                fmt.Printf("  (kernel-only)\n")
            } else if r.Presence.OrphanFS() {
                fmt.Printf("  (orphan FS)\n")
            }
        }
    }

    return nil
}
```

## Testing

### Scanner Tests

Create temp directories with fake bpffs layout:

```go
func TestScanner_ProgPins(t *testing.T) {
    dir := t.TempDir()
    os.WriteFile(filepath.Join(dir, "prog_123"), nil, 0644)
    os.WriteFile(filepath.Join(dir, "prog_456"), nil, 0644)

    scanner := bpffs.NewScanner(bpffs.ScannerDirs{FS: dir})

    var pins []bpffs.ProgPin
    for pin, err := range scanner.ProgPins(context.Background()) {
        require.NoError(t, err)
        pins = append(pins, pin)
    }

    assert.Len(t, pins, 2)
}
```

### Inspect Tests

Use fake store and kernel implementations:

```go
func TestSnapshot_ManagedPrograms(t *testing.T) {
    store := &fakeStore{
        programs: map[uint32]bpfman.Program{
            100: {ProgramName: "xdp_pass", ProgramType: bpfman.ProgramTypeXDP},
        },
    }
    kern := &fakeKernelSource{
        programs: []kernel.Program{{ID: 100}},
    }
    scanner := bpffs.NewScanner(testDirs(t))

    world, err := inspect.Snapshot(ctx, store, kern, scanner)
    require.NoError(t, err)

    managed := world.ManagedPrograms()
    assert.Len(t, managed, 1)
    assert.True(t, managed[0].Presence.InStore)
    assert.True(t, managed[0].Presence.InKernel)
}
```
