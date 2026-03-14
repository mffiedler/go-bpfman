# Derived state: what we persist and what we observe

## Principle

The store holds intent and durable handles. It does not hold state
that can be derived from the kernel, filesystem, or other observable
sources at the point of use. If a value can be read from the ground
truth at inspection time, we read it rather than storing a copy.

## Rationale

BPF state lives in multiple places simultaneously: the kernel (maps,
programs, links), the filesystem (bpffs pins, bytecode directories),
and our SQLite store (records of what we loaded and why). These
sources can and do diverge -- a program may be removed from the kernel
while its store record persists, or a pin file may be deleted while
the kernel object remains.

Storing derived values creates a maintenance burden and a consistency
problem. A stored copy of an observable fact is stale the moment the
underlying state changes. Rather than defending against staleness with
synchronisation logic, we avoid the problem entirely: the inspection
path joins the store record with live observations from the kernel and
filesystem.

This approach has several benefits:

- **Single source of truth.** There is exactly one authoritative
  source for each fact. Map pin names come from the filesystem. Kernel
  program metadata comes from the kernel. Load intent comes from the
  store. No reconciliation needed.

- **No migrations for presentation changes.** Adding a new observed
  field to the show output requires no schema change and no data
  migration. The observation is computed at display time from sources
  that already exist.

- **Correct by construction.** If a pin file exists on disk, we report
  it as present. If it does not, we report it as absent. There is no
  window in which a stale stored value could produce a wrong answer.

- **Simpler store.** The store schema remains focused on durable
  intent: what was loaded, with what parameters, by whom. It does not
  accumulate columns for every observable fact about the running
  system.

## What the store holds

The store records **intent and handles** -- the information needed to
manage the lifecycle of BPF objects:

- Program records: what was loaded, from which object file, with what
  metadata, global variables, and program type.
- Link records: what attachment was requested, to which program, with
  what parameters.
- Handles: pin paths, map directories, map owner relationships,
  bytecode locations. These are filesystem handles that the manager
  created and is responsible for cleaning up.

## What is derived at inspection time

The inspection path (`buildProgramDetail` and related formatting)
joins store records with live observations:

- **Program pin presence.** The store records the pin path; inspection
  checks whether the file exists via `os.Stat`.

- **Map pin names.** The store records the map pin directory; inspection
  reads the directory contents to discover actual pin file names. This
  avoids relying on kernel-truncated map names (the kernel caps
  `bpf_obj_name` at 15 characters, but pins use the full ELF section
  name).

- **Map pin presence.** Trivially true for pins found by directory
  listing. For kernel maps with no corresponding pin, reported as
  absent.

- **Link pin presence.** The store records the link pin path;
  inspection checks the filesystem.

- **Kernel metadata.** Program type, tag, sizes, loaded-at timestamp,
  and associated maps all come from the kernel via `bpf_prog_info` at
  query time, not from stored copies.

## The map pin name example

This principle directly resolved a bug where `show program <id> maps`
reported wrong pin paths. The old code constructed map pin paths from
the kernel-reported map name, which is truncated to 15 characters.
A map named `tracepoint_stats_map` in the ELF source was pinned under
that full name, but the kernel reported it as `tracepoint_stat`. The
constructed path did not match the real pin on disk.

The fix reads the map pin directory to discover actual file names, then
correlates each entry with a kernel map by prefix match. No schema
change, no migration, no new stored field. The filesystem is the
ground truth for "what maps were pinned," and we simply ask it.

## When to persist

Persist a value when:

- It represents intent that cannot be reconstructed (load parameters,
  user-supplied metadata, requested attachment points).
- It is a handle the manager created and must clean up (pin paths, map
  directories, bytecode directories).
- It is needed for correctness across restarts (program IDs, link IDs,
  map owner relationships).

Do not persist a value when:

- It can be read from the kernel (`bpf_prog_info`, `bpf_map_info`).
- It can be read from the filesystem (directory listings, file
  presence checks).
- It is a presentation concern (display names, formatted paths).
- Storing it would require reconciliation logic to handle divergence
  from the ground truth.
