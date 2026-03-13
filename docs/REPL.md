bpfman Typed Inspection API and Interactive Shell

Related
	-	Go rewrite of bpfman
	-	Existing coherency engine (manager/coherency/)
	-	Existing inspect package (inspect/)
	-	Existing doctor command (cmd/bpfman/doctor.go)
	-	SQLite test harness inspiration

Summary

This document proposes two things, in order of priority:

1. A typed inspection and query API for bpfman, providing structured
   read-only operations over programs, dispatchers, links, drift, and
   GC plans.

2. A minimal interactive REPL that consumes that API, giving
   developers and advanced operators an interactive surface for
   exploring state, diagnosing drift, and previewing planned actions.

The API is the primary contribution. It is useful to Go tests,
internal tools, the existing CLI, and the REPL alike. The REPL is a
thin, stateless, line-oriented frontend over those operations.

The first thing to design is not a shell. The first thing to design is
the reusable query surface. If that surface is good, the REPL is
largely a presentation problem. If that surface is weak, no amount of
shell machinery will compensate.

Background

bpfman manages and reconciles state across several layers:
	-	logical bpfman model state (the program/link/dispatcher records)
	-	persisted store state (SQLite)
	-	kernel state (loaded programs, attached links)
	-	pinned filesystem state (BPF pin paths)
	-	dispatcher state (multi-program chaining for XDP/TC)
	-	map state (shared and per-program maps)

This makes debugging and validation inherently relational. Many of the
most important questions are not about individual objects but about
cross-layer correspondence:
	-	what does bpfman think exists versus what the kernel reports?
	-	which store entries lack a corresponding live kernel object?
	-	which pin paths are present on disk but not referenced?
	-	what corrective action would bpfman take?
	-	why does this dispatcher still exist?

A one-shot CLI handles individual operations well, but it is a poor
environment for iterative questioning and cross-layer comparison.

Existing Infrastructure

The Go rewrite already contains substantial machinery for the problems
described above. This proposal builds on that existing work rather than
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
	-	inspect.GetProgram, GetLink, GetDispatcher provide
	  single-object lookups with the same correlation.

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
missing is a coherent, reusable API layer that unifies these
capabilities for multiple consumers, and an interactive surface for
exploring them.

Problem Statement

The existing infrastructure computes the right things, but the results
are consumed in ways that are tightly coupled to their current callers:

	-	Doctor results are formatted directly by the doctor command.
	-	GC plans are consumed internally by the GC executor.
	-	List/get results are formatted by CLI rendering code.
	-	The coherency engine's correlated views (ProgramState,
	  LinkState, DispatcherState) are internal to its rule
	  evaluation and not directly accessible as a query surface.

This makes several valuable activities harder than they should be:

	-	Interactively exploring the full correlated state of the system.
	-	Comparing store, kernel, and filesystem views of specific
	  objects.
	-	Inspecting what the coherency engine sees and what it would do.
	-	Previewing GC plans before execution.
	-	Writing Go tests that assert over structured inspection results.
	-	Reproducing and diagnosing drift without reading logs.

Goals

1. Define a typed inspection API that exposes structured, read-only
   queries over programs, dispatchers, links, drift, and GC plans.

2. Ensure the API returns frontend-neutral result types that can be
   rendered as text, tables, trees, or JSON by any consumer.

3. Build a minimal interactive REPL over that API with a small,
   stateless command set.

4. Make the same API usable from Go tests without the REPL.

Non-Goals

This proposal does not aim to provide:
	-	a general-purpose shell language
	-	arbitrary scripting with loops and variables
	-	embedded TCL, Lua, Python, or another foreign runtime
	-	a replacement for unit tests of pure logic
	-	a stable public scripting API from day one
	-	navigation, context stacks, or shell session state beyond
	  history and output format
	-	a graphical UI

Design Principles

API first, shell second

The typed query API is the primary deliverable. The REPL is one
consumer. Go tests are another. The CLI could become a third.

If the API is well-designed, the REPL is straightforward. If the API
is missing or weak, no shell features will compensate.

Inspection first

The API should be useful purely for reading state. Mutation support
can follow, but the immediate value lies in structured inspection and
drift diagnosis.

Stateless commands

Phase 1 commands should be stateless. Each command fully specifies its
target via selectors and flags. There is no current context, no cd,
no navigation stack. If selectors are concise and completion works
well, stateless commands are sufficient for a shallow object graph.

For example:

show program 12
show program 12 links
show program 12 maps
show dispatcher xdp nsid=1 ifindex=2
show dispatcher xdp nsid=1 ifindex=2 slots
drift
gc --dry-run

This avoids the complexity of session state management while remaining
pleasant to use interactively.

Structured results, not formatted strings

Query handlers return typed Go values. Renderers turn those values
into human or machine output. This separation is what enables JSON
output, Go test assertions, and future batch mode.

Build on existing packages

The inspect package, coherency engine, and manager already compute
most of what the API needs. The work is to define a clean query
surface over them, not to reimplement their logic.

Proposed Inspection API

This is the heart of the proposal. The API should provide typed,
read-only operations returning structured results. Multiple consumers
(REPL, CLI, Go tests) use the same operations.

The exact package layout is open, but the operations and result types
should be concrete.

Phase 1 does not expose maps as first-class top-level queries. Map
information is surfaced only through ProgramDetail and related
sub-views.

Query operations

The following operations represent the initial query surface. Each
takes typed parameters and returns a structured result.

List operations:

	-	ListPrograms(ctx, opts) -> ProgramList
	  Returns all programs with store/kernel/filesystem correlation.
	  Supports filtering by type, attachment state, labels.

	  The underlying data source is inspect.Snapshot, which already
	  performs three-source correlation. Manager.ListPrograms
	  currently builds its own composite Program values through a
	  different path. The inspection API should consolidate on
	  inspect.Snapshot as the single authoritative correlation
	  source, applying filters on top. This avoids maintaining two
	  overlapping correlation paths.

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
	  links, maps, and pin paths.
	  Builds on inspect.GetProgram.

	-	GetDispatcher(ctx, kind, nsid, ifindex) -> DispatcherDetail
	  Returns a single dispatcher with slot contents, link state,
	  revision information, and filesystem layout.
	  Builds on inspect.GetDispatcher.

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

	-	CheckCoherency(ctx, filter) -> CoherencyReport
	  Same, but filtered to specific categories (programs,
	  dispatchers, filesystem).

	-	ComputeGCPlan(ctx, opts) -> GCPreview
	  Computes what GC would do without executing. Returns
	  violations, planned actions, and expected outcomes. Backed
	  directly by Manager.ComputeGC.

	  The operation name deliberately matches the existing
	  machinery. The result type is named GCPreview to distinguish
	  the API-facing preview object from the manager's internal
	  GCPlan. If the query surface later needs to encompass
	  corrective actions beyond orphan cleanup (e.g. dispatcher
	  rebalancing or link repair), a broader operation can be
	  introduced at that point. Phase 1 should not invent a broader
	  abstraction before the need is concrete.

Result types

The result types must be frontend-neutral. They should carry enough
information for any renderer (text, table, tree, JSON) without
embedding formatting decisions.

ProgramList:
	-	ObservedAt timestamp
	-	[]ProgramSummary, each with:
	  -	ProgramID
	  -	Name, Type, License
	  -	Presence (InStore, InKernel, InFS)
	  -	LinkCount
	  -	Labels

ProgramDetail:
	-	ProgramID
	-	Record (the stored ProgramRecord)
	-	KernelInfo (the kernel.Program, if present)
	-	Presence
	-	Links []LinkSummary
	-	Maps []MapSummary
	-	PinPaths []string

DispatcherList:
	-	[]DispatcherSummary, each with:
	  -	Kind, Nsid, Ifindex
	  -	Revision
	  -	ProgramPresence, LinkPresence
	  -	SlotCount, OccupiedSlots

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

Findings represent violations only, not successful rule evaluations.
A clean system produces an empty findings list.

GCPreview:
	-	Violations []Violation (with severity and description)
	-	PlannedActions []PlannedAction, each with:
	  -	Description
	  -	ActionType
	  -	Target (object identifier)
	-	LiveOrphans count

Relationship to existing types

Many of these result types already exist or nearly exist:

	-	inspect.ProgramView is close to ProgramDetail.
	-	inspect.DispatcherRow is close to DispatcherSummary.
	-	inspect.Presence already carries InStore/InKernel/InFS.
	-	coherency.Finding is a close fit for the CoherencyReport
	  finding type and may be reused directly.
	-	coherency.Violation with its Operation is close to
	  GCPreview entries.
	-	bpfman.ProgramListResult is close to ProgramList.

API boundary choice

Phase 1 should prefer API-owned result types in a dedicated package,
even where they closely mirror existing internal types. This keeps
the query surface coherent if internal representations continue to
evolve; it also makes the boundary explicit for consumers such as Go
tests that should not depend on internal package paths.

Where an existing type is already a good fit (e.g. coherency.Finding,
inspect.Presence), the API type can embed or re-export it rather than
copying fields, provided the internal type is itself stable. The
guiding rule is: consumers of the inspection API should never need to
import manager/coherency or inspect directly.

Rendering

Rendering is separate from query execution. A renderer takes a result
type and produces output in one of:
	-	summary (human-readable prose or compact display)
	-	table (columnar, suitable for terminals)
	-	tree (hierarchical, useful for dispatchers and relationships)
	-	JSON (machine-readable, stable for automation)

The CLI already has rendering infrastructure for table, JSON, tree,
and jsonpath output. The REPL should reuse or extend that rather than
build its own.

Proposed REPL

The REPL is a thin interactive frontend over the inspection API.

Entry point

bpfman repl

Phase 1 command set

The initial command set is five command families with noun and
sub-view variants. This is deliberately small: if these families are
not useful, the REPL is not justified; if they are, expansion becomes
obvious.

	-	help
	  List available commands and their usage.

	-	list <noun>
	  List objects of a given type with store/kernel/filesystem
	  correlation.
	  Nouns: programs, dispatchers, links.
	  Supports filters (type, attachment state, labels).

	-	show <noun> <selector> [sub-view]
	  Show a single object with optional sub-view.
	  Nouns: program <id>, dispatcher <kind> nsid=<n> ifindex=<n>,
	  link <id>.
	  Sub-views: links, maps, paths, slots (noun-dependent).

	-	drift [scope]
	  Run coherency checks and display findings.
	  Optional scope: programs, dispatchers.

	-	gc --dry-run
	  Show what GC would do without executing. Renders the
	  GCPreview as violations with their planned remediation
	  actions.

What the REPL does not have in Phase 1

	-	No context, navigation, or cd/pwd.
	-	No use command or prompt state changes.
	-	No mutation beyond GC dry-run preview.
	-	No assertions or scripting.
	-	No batch file execution.

Session state is limited to command history and the current output
format setting.

Example session

bpfman> list programs
ID    NAME            TYPE         KERNEL  STORE  FS
12    xdp-pass        XDP          yes     yes    yes
15    tc-logger       TC           yes     yes    yes
20    tracepoint-dbg  Tracepoint   no      yes    yes

bpfman> show program 12
Program 12: xdp-pass
  Type:     XDP
  License:  GPL
  Kernel:   present (fd=7)
  Store:    present
  Pin:      /run/bpfman/fs/prog/12/prog
  Links:    [45]
  Maps:     [stats_map, config_map]

bpfman> show program 12 links
LINK   KIND   KERNEL  STORE  PIN
45     XDP    yes     yes    yes

bpfman> drift
SEVERITY  CATEGORY     RULE                    DESCRIPTION
warning   filesystem   orphan-fs-entries       /run/bpfman/fs/prog/99: no DB record
error     kernel       program-in-kernel       program 20: in store but not in kernel

2 findings: 1 error, 1 warning

bpfman> gc --dry-run
Violations:
  [error] program 20: in store but not in kernel
    -> DeleteProgram{ProgramID: 20}
    -> RemoveProgPin{Path: /run/bpfman/fs/prog/20/prog}

  [warning] orphan filesystem entry: /run/bpfman/fs/prog/99
    -> RemoveProgramDir{Path: /run/bpfman/fs/prog/99}

Planned actions: 3
Live orphans: 0

bpfman> show program 12 --format=json
{"program_id":12,"name":"xdp-pass","type":"XDP",...}

Output formats

All commands support --format=<fmt> where fmt is one of: summary
(default), table, tree, json.

Completion

The structured command grammar makes completion straightforward.
Completable positions include:
	-	command names (list, show, drift, gc, help)
	-	nouns after list/show (programs, dispatchers, links, program,
	  dispatcher, link)
	-	object IDs after show program / show link
	-	dispatcher selectors after show dispatcher
	-	sub-views (links, maps, paths, slots)
	-	format names after --format=

Error handling

Errors should be categorised internally even if displayed simply:
	-	parse errors (unknown command, invalid syntax)
	-	resolution errors (object not found, ambiguous selector)
	-	query errors (store or kernel access failure)

This matters for future batch mode where exit codes and structured
error output become important.

Alternatives Considered

1. Typed API only, consumed from Go tests

This is the most serious alternative. A typed inspection API consumed
directly from Go tests provides:
	-	structured results
	-	native assertions (standard Go testing)
	-	zero parser or shell work
	-	full determinism
	-	direct programmatic access

The REPL only wins on:
	-	interactivity and exploration
	-	discoverability (completion, help)
	-	manual debugging ergonomics
	-	lower barrier to entry than writing Go code

This trade-off is real. The typed API should be built regardless. The
question is whether the interactive surface justifies its additional
cost. This proposal argues it does, because the developer experience
of exploring a live system interactively is qualitatively different
from writing test assertions, and because the REPL is cheap to build
once the API exists.

2. Richer CLI, no REPL

A richer CLI with more subcommands helps with coverage, but does not
provide:
	-	interactivity and iterative exploration within a single session
	-	completion shaped by live state
	-	lower friction between adjacent queries
	-	a natural path toward batch execution and scripting

The CLI should continue to improve. The REPL complements it.

3. Embed TCL, Lua, or Starlark

Embedding a scripting language provides power but introduces:
	-	foreign runtime integration
	-	type bridging complexity
	-	surprising semantics for Go contributors
	-	maintenance burden unrelated to the domain

The right lesson from SQLite is the programmable control surface, not
the specific language choice. If scripting is ever needed, it should
come after the domain commands and result shapes have stabilised.

4. Shell scripts around the CLI

Workable for simple scenarios, but poor at:
	-	structured inspection of relational state
	-	stable querying with typed results
	-	wait/eventual assertions
	-	producing and consuming structured data cleanly

Shell scripts are a useful complement, not a replacement.

Future Evolution

Later phases can extend the REPL once the command and result surfaces
stabilise. Possible future extensions include:

	-	batch execution (bpfman repl -c '...' or --file)
	-	structured assertions over query results
	-	wait/eventual assertions for convergence testing
	-	minimal scripting features if real need emerges

These are mentioned for directional context, not as commitments. The
design of each should be deferred until the inspection API and initial
REPL have been used in practice.

Risks

Building a language too early

This is the largest risk. A parser, DSL, variables, and assertion
semantics can easily become a project of their own. The proposal
deliberately avoids that starting point.

Duplicating the CLI badly

If the REPL is just the CLI with a prompt, it will not justify its
existence. It must provide interactivity, completion, and iterative
exploration that the CLI cannot.

REPL-only backdoors

If the REPL bypasses the shared query API and accesses store or kernel
internals directly, it will diverge from real system behaviour and
become hard to maintain.

Unstable result types too early

If the API's result types are still changing rapidly, consumers
(including the REPL and any tests) will face churn. Starting with a
small surface and expanding deliberately mitigates this.

Open Questions

Which package should own the inspection API?

Options include a new top-level inspect/query package, extending the
existing inspect package, or placing the API surface in the manager.
The choice affects import paths, testability, and the boundary between
query logic and I/O.

Which object selectors should be first-class?

Programs use numeric IDs. Dispatchers use compound keys (kind, nsid,
ifindex). Links use numeric IDs. The selector syntax affects both
usability and completion.

Should the REPL expose mutation?

Inspection-first is clear, but should the REPL eventually expose load,
unload, attach, and detach? If so, should those require explicit
confirmation or a separate mode?

Implementation Strategy

Step 1: define the typed inspection API

Define the query operations and result types described above. Decide
where they live. Wire them to the existing inspect, coherency, and
manager packages.

This step is valuable even without the REPL. Go tests can immediately
consume the API.

Step 2: separate rendering from execution

Ensure that result types can be rendered as summary, table, tree, or
JSON. Reuse or extend existing CLI rendering infrastructure where
possible.

Step 3: build the minimal REPL

A line-editor loop, a command parser for the five Phase 1 command
families, completion, and rendering dispatch. This should be a small
amount of code once Steps 1 and 2 are done.

The terminal frontend (line editing, completion callbacks, history) is
expected to come from a lightweight line-editor library providing
history and completion. The choice of library is an interface concern
and not architecturally significant.

Conclusion

The primary deliverable of this proposal is not a shell. It is a
typed inspection and query API for bpfman that makes the system's
multi-layer state structured, queryable, and renderable.

That API builds on existing infrastructure: the inspect package
already correlates objects across store, kernel, and filesystem; the
coherency engine already evaluates rules and produces violations with
remediation plans; the doctor command already consumes coherency
results. What is missing is a clean, reusable query surface over these
capabilities.

Once that surface exists, a minimal interactive REPL becomes
straightforward: a thin, stateless command loop with completion over
a small set of structured queries. The REPL's value lies in
interactivity, exploration, and discoverability -- things that are
qualitatively different from writing Go tests, even when those tests
use the same API.

The recommended path:
	1.	Typed inspection API with structured result types.
	2.	Rendering layer (reusing existing CLI infrastructure).
	3.	Minimal stateless REPL with five command families.
	4.	Expansion driven by real usage, not speculative design.
