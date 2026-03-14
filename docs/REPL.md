bpfman Debugger: Typed Inspection and Interactive Shell

Related
	-	Go rewrite of bpfman
	-	Existing coherency engine (manager/coherency/)
	-	Existing inspect package (inspect/)
	-	Existing doctor command (cmd/bpfman/doctor.go)

Summary

The REPL is two things:

1. The CLI with a persistent session. Every CLI command works in it.
   You don't type "bpfman" in front. Running bpfman with no arguments
   starts the REPL, matching the convention of tools like python and
   sqlite3.

2. A debugger for bpfman's managed state. The debugger surface is
   what the one-shot CLI cannot naturally provide: multi-faceted
   inspection of a managed entity through sub-views, with the program
   ID (or dispatcher key, or link ID) as the pivot point.

The typed inspection API is the primary contribution. It is useful to
Go tests, internal tools, the existing CLI, and the REPL alike. The
REPL is a thin, stateless, line-oriented frontend over those
operations.

Current State

The REPL is partially implemented. What exists today:

Commands:
	-	help
	-	list programs / program list
	-	load file [flags]
	-	program delete <id>... [-r]

Input modes:
	-	Interactive readline with history and tab completion.
	-	File input via --file/-f (reads commands from a named file).
	-	Pipe input (auto-detected when stdin is not a terminal).
	-	Shell-style comments: '#' and everything after it is stripped.

Session:
	-	A single manager is held open for the session lifetime.
	-	Command history persists across sessions (XDG state directory).
	-	Running bpfman with no arguments starts the REPL.

Background

bpfman manages and reconciles state across several layers:
	-	logical bpfman model state (the program/link/dispatcher records)
	-	persisted store state (SQLite)
	-	kernel state (loaded programs, attached links)
	-	pinned filesystem state (BPF pin paths)
	-	dispatcher state (multi-program chaining for XDP/TC)
	-	map state (shared and per-program maps)

This makes debugging inherently relational. The most important
questions are not about individual objects but about cross-layer
correspondence:
	-	what does bpfman think exists versus what the kernel reports?
	-	which store entries lack a corresponding live kernel object?
	-	which pin paths are present on disk but not referenced?
	-	what corrective action would bpfman take?
	-	why does this dispatcher still exist?

A one-shot CLI handles individual operations well, but it is a poor
environment for iterative questioning and cross-layer comparison. The
REPL provides a persistent session where adjacent queries are cheap
and completion is shaped by live state.

Existing Infrastructure

The Go rewrite already contains substantial machinery for the problems
described above. The debugger builds on that existing work rather than
inventing a parallel subsystem.

inspect package

The inspect package (inspect/) already provides three-source
correlation. Its core function, Snapshot, gathers state from the
store, kernel, and filesystem into a single World value containing
correlated views:

	-	inspect.World holds []ProgramView, []LinkRow,
	  []DispatcherRow, and SnapshotMeta.
	-	inspect.ProgramView correlates a program across store, kernel,
	  and filesystem, with a Presence struct indicating where the
	  object exists.
	-	inspect.LinkRow does the same for links.
	-	inspect.DispatcherRow does the same for dispatchers.
	-	inspect.GetProgram, GetLink provide single-object lookups
	  with the same correlation.

The inspect package uses narrow interfaces (StoreLister,
KernelLister, etc.) for dependency injection.

coherency engine

The coherency package (manager/coherency/) implements a declarative
rule engine that evaluates observed state and produces violations with
remediation operations:

	-	coherency.GatherState builds an ObservedState snapshot with
	  correlated views (Programs(), Links(), Dispatchers(),
	  OrphanFsEntries()) plus derived facts like link counts and TC
	  filter checks.
	-	coherency.Rule is a named, described evaluation function over
	  ObservedState producing []Violation.
	-	coherency.Violation carries a severity, category, rule name,
	  description, and an optional Operation containing
	  []action.Action for remediation.
	-	CoherencyRules() returns 14 doctor-oriented rules covering
	  kernel presence, pin presence, orphan filesystem entries,
	  dispatcher link counts, and more.
	-	GCRules() returns rules for automated garbage collection.

doctor command

The doctor command (cmd/bpfman/doctor.go) already consumes the
coherency engine:

	-	doctor checkup calls Manager.Doctor, which runs
	  CoherencyRules over gathered state and returns a DoctorReport
	  containing []Finding with severity, category, rule name, and
	  description.
	-	doctor explain lists available rules or describes a specific
	  rule.

GC and dry-run planning

The manager already supports dry-run planning for GC:

	-	Manager.ComputeGC returns a GCPlan containing
	  []action.Action, []coherency.Violation, and a live orphan
	  count -- without executing anything.
	-	Manager.GCWithOptions executes the plan.
	-	Actions are reified values from the action package
	  (DeleteDispatcher, DeleteProgram, DeleteLink,
	  RemoveProgPin, etc.), not imperative calls.

existing CLI queries

The CLI already provides structured queries through the manager:

	-	Manager.ListPrograms returns ProgramListResult with
	  []Program (each being a ProgramRecord plus ProgramStatus).
	-	Manager.Get returns a single composite Program.
	-	Manager.GetLinkInfo returns an inspect.LinkInfo.
	-	Output formats include JSON, table, wide, tree, and jsonpath.

What is missing is not data collection or rule evaluation. What is
missing is a debugger surface that lets you pivot on a managed entity
and see it from multiple angles.

Design Principles

CLI parity

Every CLI command should work in the REPL. The REPL is the CLI with
a persistent session, readline, and completion. If you can type
"bpfman program list", you can type "program list" at the REPL
prompt.

Inspection through sub-views

The debugger surface is the `show` command with sub-views. A program
ID is the pivot point; sub-views fan out to different facets of the
same entity:

show program 12
show program 12 links
show program 12 maps
show program 12 paths
show dispatcher xdp nsid=1 ifindex=2
show dispatcher xdp nsid=1 ifindex=2 slots
drift
gc --dry-run

Each sub-view is a rendering choice over the same underlying data,
not a different query. The typed result carries everything; the
sub-view selects what to present.

Stateless commands

Commands are stateless. Each command fully specifies its target via
selectors and flags. There is no current context, no cd, no
navigation stack. If selectors are concise and completion works well,
stateless commands are sufficient for a shallow object graph.

Structured results, not formatted strings

Query handlers return typed Go values. Renderers turn those values
into human or machine output. This separation is what enables JSON
output, Go test assertions, and batch mode.

Build on existing packages

The inspect package, coherency engine, and manager already compute
most of what the debugger needs. The work is to define a clean query
surface over them, not to reimplement their logic.

Inspection API

The heart of the debugger is the typed inspection API. It provides
read-only operations returning structured results. Multiple consumers
(REPL, CLI, Go tests) use the same operations.

Query operations

List operations:

	-	ListPrograms(ctx, opts) -> ProgramList
	  Returns all programs with store/kernel/filesystem correlation.
	  Supports filtering by type, attachment state, labels.

	  The underlying data source is inspect.Snapshot, which already
	  performs three-source correlation. The inspection API
	  consolidates on inspect.Snapshot as the single authoritative
	  correlation source, applying filters on top.

	-	ListDispatchers(ctx) -> DispatcherList
	  Returns all dispatchers with store/kernel/filesystem
	  correlation, slot occupancy, and link counts.
	  Builds on inspect.World.Dispatchers.

	-	ListLinks(ctx, opts) -> LinkList
	  Returns all links with store/kernel correlation.
	  Builds on inspect.World.Links.

Single-object operations:

	-	GetProgram(ctx, id) -> ProgramDetail
	  Returns a single program with full correlation plus associated
	  links, maps (owned and shared), and pin paths with presence.
	  Builds on inspect.GetProgram.

	-	GetDispatcher(ctx, kind, nsid, ifindex) -> DispatcherDetail
	  Returns a single dispatcher with slot contents, link state,
	  revision information, and filesystem layout.

	-	GetLink(ctx, id) -> LinkDetail
	  Returns a single link with store/kernel correlation and
	  attachment details.
	  Builds on inspect.GetLink.

Drift and coherency operations:

	-	CheckCoherency(ctx) -> CoherencyReport
	  Runs the coherency rules and returns structured findings
	  grouped by category and severity.
	  Builds on Manager.Doctor / coherency.Evaluate with
	  CoherencyRules.

	-	ComputeGCPlan(ctx, opts) -> GCPreview
	  Computes what GC would do without executing. Returns
	  violations, planned actions, and expected outcomes. Backed
	  directly by Manager.ComputeGC.

Result types

The result types must be frontend-neutral. They carry enough
information for any renderer (text, table, tree, JSON) without
embedding formatting decisions.

ProgramDetail is the key type for the debugger. It is the pivot
result: everything reachable from a program ID.

ProgramDetail:
	-	ProgramID
	-	Record (the stored ProgramRecord)
	-	KernelInfo (the kernel.Program, if present)
	-	Presence (InStore, InKernel, InFS)
	-	Links []LinkSummary (with per-link pin path and presence)
	-	Maps []MapInfo (ID, name, type, key/value sizes, pin path,
	  presence, whether shared)
	-	Pins: programme pin path and presence, map directory path
	  and presence, link directory path and presence, bytecode
	  path and presence

ProgramList:
	-	ObservedAt timestamp
	-	[]ProgramSummary, each with:
	  -	ProgramID
	  -	Name, Type, License
	  -	Presence (InStore, InKernel, InFS)
	  -	LinkCount
	  -	Labels

DispatcherDetail:
	-	Kind, Nsid, Ifindex
	-	Revision
	-	Presence (program pin, link pin, kernel)
	-	Slots []SlotInfo (priority, program ID, link state)
	-	FSLinkCount

CoherencyReport:
	-	ObservedAt timestamp
	-	Findings []Finding, each with:
	  -	Severity (Warning, Error)
	  -	Category, RuleName, Description
	-	Summary (error count, warning count)

GCPreview:
	-	Violations []Violation (with severity and description)
	-	PlannedActions []PlannedAction, each with:
	  -	Description
	  -	ActionType
	  -	Target (object identifier)
	-	LiveOrphans count

Relationship to existing types

Many of these result types already exist or nearly exist:

	-	inspect.ProgramView is close to ProgramDetail but lacks
	  per-map pin path correlation and shared map information.
	-	inspect.DispatcherRow is close to DispatcherSummary.
	-	inspect.Presence already carries InStore/InKernel/InFS.
	-	coherency.Finding is a close fit for the CoherencyReport
	  finding type and may be reused directly.
	-	bpfman.ProgramListResult is close to ProgramList.

Rendering

Rendering is separate from query execution. A renderer takes a result
type and produces output in one of:
	-	summary (human-readable, the default for show)
	-	table (columnar, the default for list)
	-	tree (hierarchical, useful for dispatchers and relationships)
	-	JSON (machine-readable, stable for automation)

Sub-views are rendering selections, not distinct queries. "show
program 12 maps" runs the same GetProgram query as "show program 12"
but renders only the maps facet.

The CLI already has rendering infrastructure for table, JSON, tree,
and jsonpath output. The REPL reuses and extends that.

Example Session

bpfman> list programs
PROGRAM ID  TYPE        NAME                      SOURCE
195043      tracepoint  tracepoint_kill_recorder  /run/bpfman/programs/195043/bytecode.o

bpfman> show program 195043
Program 195043: tracepoint_kill_recorder (tracepoint)
  Source:     /run/bpfman/programs/195043/bytecode.o
  Loaded:     2026-03-13T19:42:01Z
  Tag:        a1b2c3d4e5f6
  JIT: 1284B  Xlated: 896B  Memlock: 4096B
  Presence:   store=yes  kernel=yes  fs=yes

  Maps (2):    kill_events, rb_events
  Links (1):   8801 (sched/sched_switch)

bpfman> show program 195043 maps
ID     NAME           TYPE       KEYS   VALUES  MAX   PIN                                            PRESENT
4201   kill_events    ringbuf    0B     0B      256   /sys/fs/bpf/bpfman/maps/195043/kill_events    yes
4202   rb_events      ringbuf    0B     0B      256   /sys/fs/bpf/bpfman/maps/195043/rb_events      yes

bpfman> show program 195043 links
ID     TYPE          ATTACH                PIN                                                       PRESENT
8801   tracepoint    sched/sched_switch    /sys/fs/bpf/bpfman/links/195043/sched_sched_switch       yes

bpfman> show program 195043 paths
/sys/fs/bpf/bpfman/prog_195043                              present
/sys/fs/bpf/bpfman/maps/195043/                             present (2 pins)
/sys/fs/bpf/bpfman/maps/195043/kill_events                  present
/sys/fs/bpf/bpfman/maps/195043/rb_events                    present
/sys/fs/bpf/bpfman/links/195043/                            present (1 pin)
/sys/fs/bpf/bpfman/links/195043/sched_sched_switch          present
/run/bpfman/programs/195043/bytecode.o                       present

bpfman> drift
SEVERITY  CATEGORY     RULE                    DESCRIPTION
warning   filesystem   orphan-fs-entries       /sys/fs/bpf/bpfman/prog_99: no DB record
error     kernel       program-in-kernel       program 20: in store but not in kernel

2 findings: 1 error, 1 warning

bpfman> gc --dry-run
Violations:
  [error] program 20: in store but not in kernel
    -> DeleteProgram{ProgramID: 20}
    -> RemoveProgPin{Path: /sys/fs/bpf/bpfman/prog_20}

  [warning] orphan filesystem entry: /sys/fs/bpf/bpfman/prog_99
    -> RemoveProgramDir{Path: /sys/fs/bpf/bpfman/prog_99}

Planned actions: 3
Live orphans: 0

Completion

The structured command grammar makes completion straightforward.
Completable positions include:
	-	command names (list, show, load, program, link, etc.)
	-	nouns after list/show (programs, dispatchers, links, program,
	  dispatcher, link)
	-	object IDs after show program / show link (from live state)
	-	sub-views (links, maps, paths, slots)
	-	flags and their values

Implementation Strategy

Step 1: show program with sub-views

Build the `show program <id>` command in the REPL with sub-views:
summary (default), links, maps, paths. This requires enriching the
GetProgram result to include per-map and per-link pin path
correlation, which inspect.GetProgram currently does not carry.

The building blocks:
	-	inspect.GetProgram provides the correlated ProgramView with
	  presence.
	-	manager.Get already fetches kernel maps, links, and stats.
	-	fs.Scanner.PathExists can check individual pins.
	-	fs.BPFFS knows the path conventions (MapPinPath,
	  LinkPinPath, ProgPinPath).

The work is to combine these into a ProgramDetail result and write
renderers for each sub-view.

Step 2: CLI parity

Wire every existing CLI command into the REPL dispatcher so that
the REPL is a complete superset of the CLI. This is mechanical:
parse the REPL input line and delegate to the same execution path
that the CLI uses.

Step 3: drift and gc --dry-run

Surface the coherency engine and GC planner through the REPL.
These are already implemented in the manager; the work is wiring
them into the REPL command set and writing renderers for
CoherencyReport and GCPreview.

Step 4: show dispatcher and show link

Extend the sub-view pattern to dispatchers (with slots sub-view)
and links.

Non-Goals

	-	A general-purpose shell language.
	-	Loops, conditionals, or control flow.
	-	Embedded TCL, Lua, Python, or another foreign runtime.
	-	Navigation, context stacks, or cd/pwd.
	-	A graphical UI.

The REPL supports explicit variable assignment and structured field
access so that users can capture command results and feed them to
later commands (see docs/REPL-LANG.md). This is the minimum
language support needed for lifecycle scripting. Beyond that, the
REPL is a debugger, not a scripting environment. It inspects managed
state through structured, typed queries with multiple rendering
views.
