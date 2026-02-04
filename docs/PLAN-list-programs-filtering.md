# Plan: Filtering Options for `list programs`

## Overview

This document outlines a plan to add filtering capabilities to the
`bpfman list programs` command, enabling users to narrow down results
based on attachment state, program type, and metadata labels.

## Motivation

Currently, `list programs` returns all BPF programs without any
filtering capability. As systems accumulate more BPF programs, users
need ways to:

- Quickly identify programs that are attached vs those that are loaded
  but not actively attached
- Filter by program type when debugging specific subsystems
- Query programs by metadata labels, similar to how kubectl filters
  Kubernetes resources

## Proposed Filters

### 1. Attachment State Filters

**Flags:** `--attached`, `--unattached`

Filter programs based on whether they have active link attachments.

```
bpfman list programs --attached      # Only programs with links
bpfman list programs --unattached    # Only programs without links
```

**Implementation Notes:**
- A program is "attached" if it has at least one entry in the links
  registry
- These flags are mutually exclusive
- Default behaviour (no flag) shows all programs

**Use Cases:**
- Identify orphaned programs that were loaded but never attached
- Find all actively running BPF programs
- Clean-up operations targeting unattached programs

### 2. Program Type Filter

**Flag:** `--type=<type>`

Filter programs by their BPF program type.

```
bpfman list programs --type=tracepoint
bpfman list programs --type=kprobe
bpfman list programs --type=xdp
```

**Implementation Notes:**
- Accept the same type names used in other commands and output
- Case-insensitive matching
- Support comma-separated values for multiple types:
  `--type=kprobe,uprobe`

**Use Cases:**
- Focus on specific subsystems (e.g., all XDP programs)
- Debugging attachment issues for a particular program type
- Auditing programs by category

### 3. Label Selector Filter

**Flag:** `-l` / `--selector=<selector>`

Filter programs by metadata labels using kubectl-style selectors.

```
bpfman list programs -l app=myapp
bpfman list programs -l "app=myapp,version=v2"
bpfman list programs -l "app in (foo,bar)"
bpfman list programs -l "!debug"
```

**Selector Syntax (kubectl-compatible):**
- `key=value` - equality match
- `key!=value` - inequality match
- `key` - label exists
- `!key` - label does not exist
- `key in (v1,v2)` - value in set
- `key notin (v1,v2)` - value not in set
- Multiple selectors separated by commas (AND logic)

**Implementation Notes:**
- Requires a label selector parser (consider using or adapting
  k8s.io/apimachinery/pkg/labels or writing a minimal parser)
- Labels are stored in the SQLite metadata table
- Filtering can happen at the SQL level for efficiency or
  post-retrieval for simpler implementation

**Use Cases:**
- Find all programs belonging to a specific application
- Query programs by environment (dev/staging/prod)
- Locate programs with specific feature flags or versions

## Implementation Approach

### Phase 1: Core Infrastructure

1. Add filter types to the domain layer:
   ```go
   type ListProgramsFilter struct {
       AttachmentState *AttachmentState // nil = all, Attached, Unattached
       Types           []ProgramType    // empty = all types
       LabelSelector   labels.Selector  // nil = no label filtering
   }
   ```

2. Extend `Store.ListPrograms()` to accept filter options:
   ```go
   ListPrograms(ctx context.Context, opts ...ListOption) ([]ProgramSpec, error)
   ```

3. Implement SQL-level filtering where efficient (type, attachment
   state) and post-retrieval filtering where necessary (complex label
   selectors).

### Phase 2: Attachment State Filter

1. Add `--attached` and `--unattached` flags to CLI
2. Implement SQL JOIN/LEFT JOIN logic to filter by link presence
3. Add tests for both states

### Phase 3: Type Filter

1. Add `--type` flag to CLI
2. Implement WHERE clause filtering on program type
3. Support comma-separated values
4. Add tests

### Phase 4: Label Selector

1. Evaluate label selector parsing options:
   - Option A: Use k8s.io/apimachinery/pkg/labels (adds dependency)
   - Option B: Write minimal parser supporting common operations
   - Option C: Start with simple key=value only, extend later

2. Add `-l` / `--selector` flag to CLI
3. Implement filtering (SQL-level or post-retrieval)
4. Add comprehensive tests for selector syntax

## Considerations

### Filter Combination

All filters should combine with AND logic:
```
bpfman list programs --attached --type=xdp -l app=myapp
```
Returns XDP programs that are attached AND have label app=myapp.

### Output Format Compatibility

Filters should work with all output formats:
- Default table output
- Wide output (`-o wide`)
- Custom columns (`-o custom-columns=...`)
- JSON output (if added)

### Performance

- Attachment state: Single JOIN, minimal overhead
- Type filter: Simple WHERE clause, negligible overhead
- Label selector: May require multiple JOINs on metadata table;
  consider indexing strategy if performance becomes an issue

### Error Handling

- Invalid type names: Clear error with list of valid types
- Invalid selector syntax: Parse error with position indicator
- Mutually exclusive flags: Clear error message

## Future Extensions

- `--namespace` filter if namespace support is added
- `--name` filter with glob/regex matching
- `--id` filter for specific program IDs
- Negative type filter: `--type=!kprobe`
- Output sorting options: `--sort-by=type`

## References

- kubectl label selectors: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
- Current `list programs` implementation: `cmd/list.go`, `cmd/list_programs.go`
- Store interface: `interpreter/store/store.go`
