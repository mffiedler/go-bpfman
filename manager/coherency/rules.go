package coherency

import (
	"fmt"

	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/manager/action"
)

// Evaluate runs all rules against the observed state and returns
// violations found. Each violation is stamped with the rule's name.
func Evaluate(state *ObservedState, rules []Rule) []Violation {
	var out []Violation
	for _, rule := range rules {
		vs := rule.Eval(state)
		for i := range vs {
			vs[i].RuleName = rule.Name
		}
		out = append(out, vs...)
	}
	return out
}

// AllRules returns all rules (doctor + GC) for introspection.
func AllRules() []Rule {
	return append(CoherencyRules(), GCRules()...)
}

// FindRule returns the rule with the given name, or nil if not found.
func FindRule(name string) *Rule {
	for _, r := range AllRules() {
		if r.Name == name {
			return &r
		}
	}
	return nil
}

// RuleNames returns the names of all rules.
func RuleNames() []string {
	rules := AllRules()
	names := make([]string, len(rules))
	for i, r := range rules {
		names[i] = r.Name
	}
	return names
}

// CoherencyRules returns all doctor rules.
func CoherencyRules() []Rule {
	return []Rule{
		// Warn if kernel enumeration was incomplete.
		{
			Name: "kernel-enumeration-incomplete",
			Description: `Reports when bpfman failed to enumerate all kernel BPF programs
or links. This can happen due to permission errors or kernel bugs.
When enumeration is incomplete, other coherency checks may miss
violations because they lack full visibility into kernel state.

Severity: WARNING
Category: enumeration`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				progErrors := s.world.Meta.ProgramEnumErrors
				linkErrors := s.world.Meta.LinkEnumErrors
				if progErrors+linkErrors > 0 {
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "enumeration",
						Description: fmt.Sprintf("Kernel enumeration incomplete (%d program errors, %d link errors); results may be partial", progErrors, linkErrors),
					})
				}
				return out
			},
		},
		// Each DB program must have a corresponding kernel program.
		{
			Name: "program-in-kernel",
			Description: `Checks that every program recorded in the database has a
corresponding kernel BPF program. A mismatch means the database
references a program that no longer exists in the kernel - it was
unloaded externally or the kernel reclaimed it.

This is an error because the database is out of sync with reality.
Operations referencing this program will fail.

Severity: ERROR
Category: db-vs-kernel`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, p := range s.Programs() {
					if p.DB != nil && !p.Kernel {
						out = append(out, Violation{
							Severity:    SeverityError,
							Category:    "db-vs-kernel",
							Description: fmt.Sprintf("Program %d in DB not found in kernel (pin: %s)", p.ProgramID, p.PinPath),
						})
					}
				}
				return out
			},
		},
		// Each DB link must have a corresponding kernel link.
		{
			Name: "link-in-kernel",
			Description: `Checks that every BPF link recorded in the database has a
corresponding kernel link. A mismatch means the link was detached
externally. Synthetic link IDs (used for perf_event attachments) are
skipped since they are not enumerable via the kernel iterator.

This is an error because the database believes a link exists that
the kernel has already removed.

Severity: ERROR
Category: db-vs-kernel`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, l := range s.Links() {
					if l.Synthetic || l.DB == nil {
						continue
					}
					if !l.Kernel {
						// For non-synthetic links, ID is the kernel link ID
						out = append(out, Violation{
							Severity:    SeverityError,
							Category:    "db-vs-kernel",
							Description: fmt.Sprintf("Link %d in DB not found in kernel", l.DB.ID),
						})
					}
				}
				return out
			},
		},
		// Each DB dispatcher must have a corresponding kernel program.
		{
			Name: "dispatcher-prog-in-kernel",
			Description: `Checks that every dispatcher recorded in the database has its
kernel program still loaded. A dispatcher is a BPF program that
multiplexes multiple user programs through tail calls.

If the dispatcher program is gone, the entire dispatch chain is
broken and no attached programs can run.

Severity: ERROR
Category: db-vs-kernel`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.DB != nil && !d.KernelProg {
						out = append(out, Violation{
							Severity:    SeverityError,
							Category:    "db-vs-kernel",
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: program %d not found in kernel", d.DB.Type, d.DB.Nsid, d.DB.Ifindex, d.DB.ProgramID),
						})
					}
				}
				return out
			},
		},
		// Each XDP dispatcher with a link ID must have a corresponding kernel link.
		{
			Name: "xdp-link-in-kernel",
			Description: `Checks that every XDP dispatcher has its BPF link still active in
the kernel. XDP dispatchers use BPF links to attach to network
interfaces.

If the link is gone, the dispatcher program is loaded but not
attached - packets bypass it entirely.

Severity: ERROR
Category: db-vs-kernel`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.DB != nil && d.DB.Type == dispatcher.DispatcherTypeXDP && d.DB.LinkID != 0 && !d.KernelLink {
						out = append(out, Violation{
							Severity:    SeverityError,
							Category:    "db-vs-kernel",
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: link %d not found in kernel", d.DB.Type, d.DB.Nsid, d.DB.Ifindex, d.DB.LinkID),
						})
					}
				}
				return out
			},
		},
		// Each TC dispatcher must have a netlink filter installed.
		// A missing filter is only an ERROR when the dispatcher has
		// active extension links — it should be routing traffic but
		// cannot. With zero extensions the dispatcher is functionally
		// dead and the missing filter is merely a WARNING (stale
		// state eligible for GC, not a correctness failure).
		{
			Name: "tc-filter-exists",
			Description: `Checks that every TC dispatcher has its netlink filter installed.
TC dispatchers (ingress/egress) use netlink filters rather than BPF
links to attach to network interfaces.

If the filter is missing and the dispatcher has active extensions,
this is an ERROR - traffic should be routed through the dispatcher
but cannot be. If there are no extensions, it is a WARNING indicating
stale state eligible for garbage collection.

Severity: ERROR (with extensions) or WARNING (without)
Category: db-vs-kernel`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.TCFilterOK != nil && !*d.TCFilterOK {
						sev := SeverityWarning
						if d.LinkCount > 0 {
							sev = SeverityError
						}
						out = append(out, Violation{
							Severity:    sev,
							Category:    "db-vs-kernel",
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: TC filter not found (priority %d)", d.DB.Type, d.DB.Nsid, d.DB.Ifindex, d.DB.Priority),
						})
					}
				}
				return out
			},
		},
		// Each DB program with a pin path must have the pin on the filesystem.
		{
			Name: "program-pin-exists",
			Description: `Checks that every program in the database has its pin file on the
bpffs filesystem. Pin files keep BPF programs alive and addressable
by path.

A missing pin means the program may be unloaded by the kernel when
its reference count drops to zero. This is a warning because the
program might still be running (held by other references).

Severity: WARNING
Category: db-vs-fs`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, p := range s.Programs() {
					if p.PinExist != nil && !*p.PinExist {
						out = append(out, Violation{
							Severity:    SeverityWarning,
							Category:    "db-vs-fs",
							Description: fmt.Sprintf("Program %d: pin path missing: %s", p.ProgramID, p.PinPath),
						})
					}
				}
				return out
			},
		},
		// Each DB link with a pin path must have the pin on the filesystem.
		{
			Name: "link-pin-exists",
			Description: `Checks that every BPF link in the database has its pin file on the
bpffs filesystem. Pin files keep links alive and prevent automatic
detachment.

A missing pin means the link may be detached when its reference count
drops. Synthetic link IDs (for perf_event attachments) are skipped.

Severity: WARNING
Category: db-vs-fs`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, l := range s.Links() {
					if l.Synthetic || l.DB == nil {
						continue
					}
					if l.PinExist != nil && !*l.PinExist {
						pinStr := ""
						if l.DB.PinPath != nil {
							pinStr = l.DB.PinPath.String()
						}
						out = append(out, Violation{
							Severity:    SeverityWarning,
							Category:    "db-vs-fs",
							Description: fmt.Sprintf("Link %d: pin path missing: %s", l.DB.ID, pinStr),
						})
					}
				}
				return out
			},
		},
		// Each DB dispatcher must have its prog pin on the filesystem.
		{
			Name: "dispatcher-prog-pin-exists",
			Description: `Checks that every dispatcher has its program pin file on the bpffs.
The dispatcher program pin is located at:
  {bpffs}/{type}/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher

A missing pin indicates the dispatcher may be unloaded, breaking the
entire dispatch chain for that interface.

Severity: WARNING
Category: db-vs-fs`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.ProgPinExist != nil && !*d.ProgPinExist {
						out = append(out, Violation{
							Severity:    SeverityWarning,
							Category:    "db-vs-fs",
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: prog pin missing: %s", d.DB.Type, d.DB.Nsid, d.DB.Ifindex, d.ProgPin),
						})
					}
				}
				return out
			},
		},
		// Each XDP dispatcher must have its link pin on the filesystem.
		{
			Name: "xdp-link-pin-exists",
			Description: `Checks that every XDP dispatcher has its link pin file on the bpffs.
The link pin is located at:
  {bpffs}/xdp/dispatcher_{nsid}_{ifindex}_link

A missing link pin means the XDP attachment may be released, causing
the dispatcher to detach from the interface.

Severity: WARNING
Category: db-vs-fs`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.LinkPinExist != nil && !*d.LinkPinExist {
						out = append(out, Violation{
							Severity:    SeverityWarning,
							Category:    "db-vs-fs",
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: link pin missing", d.DB.Type, d.DB.Nsid, d.DB.Ifindex),
						})
					}
				}
				return out
			},
		},
		// Filesystem entries with no corresponding DB record are orphans.
		// Skip live prog-pins here; they're reported by the more specific
		// kernel-program-pinned-but-not-in-db rule with EBUSY context.
		{
			Name: "orphan-fs-entries",
			Description: `Reports filesystem entries under the bpffs tree that have no
corresponding database record. These include:
  - prog-pin: orphan program pin files (dead programs only)
  - link-dir: orphan link directories
  - map-dir: orphan map directories
  - dispatcher-dir: orphan dispatcher revision directories
  - dispatcher-link: orphan dispatcher link pins

Orphans waste filesystem space and may indicate incomplete cleanup
from a previous crash or failed operation.

Live program pins are reported separately by the
kernel-program-pinned-but-not-in-db rule with EBUSY risk context.

Severity: WARNING
Category: fs-vs-db`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					// Skip live prog-pins - reported by kernel-program-pinned-but-not-in-db.
					if o.Kind == OrphanProgPin && o.ProgramID != 0 && s.KernelAlive(o.ProgramID) {
						continue
					}
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "fs-vs-db",
						Description: fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
					})
				}
				return out
			},
		},
		// Kernel program pinned under bpfman root but not tracked in DB.
		// This is a distinct check from orphan-fs-entries: it specifically
		// identifies cases where a live kernel program is pinned under
		// our root but has no DB record. This can cause EBUSY when
		// attempting to attach to the same interface.
		{
			Name: "kernel-program-pinned-but-not-in-db",
			Description: `Reports kernel BPF programs that are:
  1. Pinned under bpfman's bpffs root (e.g., /run/bpfman/fs/prog_*)
  2. Still alive in the kernel
  3. Not tracked in bpfman's database

These are "live orphans" - programs that bpfman likely created but
has lost track of (e.g., after database deletion or corruption).

EBUSY RISK: If such a program is a dispatcher occupying a hook point
(XDP, TC), attempting to attach a new program to the same interface
will fail with EBUSY because the hook is already occupied.

GC will not remove these because removing the pin would unload a
running program. Manual cleanup: rm /run/bpfman/fs/prog_XXXXX

Severity: WARNING
Category: kernel-vs-db`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					if o.Kind != OrphanProgPin || o.ProgramID == 0 {
						continue
					}
					if !s.KernelAlive(o.ProgramID) {
						continue // not a live EBUSY risk
					}
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "kernel-vs-db",
						Description: fmt.Sprintf("Kernel program %d is pinned under %s but not tracked in DB; may cause EBUSY", o.ProgramID, o.Path),
					})
				}
				return out
			},
		},
		// Orphan program directories under <base>/programs/ with no DB record.
		{
			Name: "orphan-program-dirs",
			Description: `Reports orphan program directories under <base>/programs/ that have
no corresponding database record. These directories contain persisted
bytecode from a previous load that was rolled back, crashed, or whose
DB row was removed. GC will remove them.

Severity: WARNING
Category: fs-vs-db`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					if o.Kind != OrphanProgramDir && o.Kind != OrphanProgramDirUnk {
						continue
					}
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "fs-vs-db",
						Description: fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
					})
				}
				return out
			},
		},
		// DB dispatcher link count must match the filesystem link count.
		{
			Name: "dispatcher-link-count",
			Description: `Checks that the number of extension links recorded in the database
matches the number of link_* files in the dispatcher's revision
directory on the filesystem.

A mismatch indicates inconsistent state - either a link was added
without updating the filesystem, or a filesystem entry was removed
without updating the database.

Severity: WARNING
Category: consistency`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.LinkCount < 0 {
						continue
					}
					fsCount := s.DispatcherFsLinkCount(d)
					if fsCount < 0 {
						continue
					}
					if d.LinkCount != fsCount {
						out = append(out, Violation{
							Severity:    SeverityWarning,
							Category:    "consistency",
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: DB link count (%d) != filesystem link count (%d)", d.DB.Type, d.DB.Nsid, d.DB.Ifindex, d.LinkCount, fsCount),
						})
					}
				}
				return out
			},
		},
	}
}

// GCRules returns rules that detect and plan repairs for stale state.
func GCRules() []Rule {
	rules := []Rule{
		// Dispatchers with zero extension links and missing attachment
		// mechanism (prog pin or TC filter) are functionally dead.
		{
			Name: "stale-dispatcher",
			Description: `Detects dispatchers that are functionally dead and can be removed.
A dispatcher is stale when:
  1. It has zero extension links (no programs attached), AND
  2. Its attachment mechanism is missing (prog pin gone, or TC filter
     not installed)

Such dispatchers serve no purpose - they have no programs to dispatch
and cannot receive traffic anyway. GC removes the database record and
cleans up any remaining filesystem artefacts.

Severity: WARNING
Category: gc-dispatcher`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, d := range s.Dispatchers() {
					if d.DB == nil || d.LinkCount > 0 {
						continue
					}
					stale := false
					if d.ProgPinExist != nil && !*d.ProgPinExist {
						stale = true // G5: prog pin missing
					} else if d.TCFilterOK != nil && !*d.TCFilterOK {
						stale = true // G6: TC filter missing
					}
					if !stale {
						continue
					}
					actions := []action.Action{
						action.RemoveDispatcherProgPin{Path: d.ProgPin},
						action.RemoveDispatcherRevDir{Path: d.RevDir},
					}
					if d.DB.Type == dispatcher.DispatcherTypeXDP {
						actions = append(actions, action.RemoveDispatcherLinkPin{Path: d.LinkPin})
					}
					actions = append(actions, action.DeleteDispatcher{
						Type: d.DB.Type, Nsid: d.DB.Nsid, Ifindex: d.DB.Ifindex,
					})
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-dispatcher",
						Description: fmt.Sprintf("Stale dispatcher %s nsid=%d ifindex=%d: no extensions, functionally dead", d.DB.Type, d.DB.Nsid, d.DB.Ifindex),
						Op: &Operation{
							Description: fmt.Sprintf("delete dispatcher %s/%d/%d and filesystem artefacts", d.DB.Type, d.DB.Nsid, d.DB.Ifindex),
							Actions:     actions,
						},
					})
				}
				return out
			},
		},
	}

	for _, spec := range orphanGCSpecs {
		rules = append(rules, orphanRule(spec))
	}

	return rules
}

// orphanGCSpec describes a table-driven orphan GC rule. Each spec
// produces a Rule whose Eval iterates OrphanFsEntries, filters by
// kind and liveness, and emits violations with removal actions.
type orphanGCSpec struct {
	name        string
	description string
	kinds       []OrphanKind
	include     func(*ObservedState, FsOrphan) bool
	actionFn    func(FsOrphan) action.Action
	describeFn  func(FsOrphan) (violation, opDesc string)
}

// orphanGCSpecs defines the four orphan GC rules. Each is
// distinguished by which orphan kinds it handles, its liveness
// filter, and how it maps an orphan to a removal action.
var orphanGCSpecs = []orphanGCSpec{
	{
		name: "orphan-program-artefacts",
		description: `Removes orphan filesystem artefacts for programs that are no longer
alive in the kernel. This includes:
  - prog_* pin files (when the kernel program is dead)
  - link directories under fs/links/
  - map directories under fs/maps/

These artefacts have no database record and no live kernel object,
so they serve no purpose and waste filesystem space.

Note: Live program pins (kernel program still running) are NOT
removed - doing so would unload the program.

Severity: WARNING
Category: gc-orphan-pin`,
		kinds: []OrphanKind{OrphanProgPin, OrphanLinkDir, OrphanMapDir},
		include: func(s *ObservedState, o FsOrphan) bool {
			return o.ProgramID == 0 || !s.KernelAlive(o.ProgramID)
		},
		actionFn: orphanPinAction,
		describeFn: func(o FsOrphan) (string, string) {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
				fmt.Sprintf("remove %s", o.Path)
		},
	},
	{
		name: "orphan-dispatcher-artefacts",
		description: `Removes orphan dispatcher filesystem artefacts that have no
corresponding database record. This includes:
  - dispatcher_* revision directories
  - dispatcher_*_link pin files

These artefacts indicate a dispatcher that was partially cleaned up
or created by a different bpfman instance. They waste filesystem
space and can cause confusion.

Severity: WARNING
Category: gc-orphan-pin`,
		kinds:   []OrphanKind{OrphanDispatcherDir, OrphanDispatcherLink},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		actionFn: func(o FsOrphan) action.Action {
			switch o.Kind {
			case OrphanDispatcherDir:
				return action.RemoveDispatcherRevDir{Path: o.Path}
			case OrphanDispatcherLink:
				return action.RemoveDispatcherLinkPin{Path: o.Path}
			default:
				panic(fmt.Sprintf("orphan-dispatcher-artefacts: unexpected kind %s", o.Kind))
			}
		},
		describeFn: func(o FsOrphan) (string, string) {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
				fmt.Sprintf("remove %s", o.Path)
		},
	},
	{
		name: "orphan-program-dirs",
		description: `Removes orphan program directories under <base>/programs/ that
have no corresponding database record. These directories contain
persisted bytecode and provenance from a previous load that was
either rolled back, crashed, or whose DB row was removed.

Both numeric (program-dir) and non-numeric (program-dir-unknown)
directory names are removed. All entries under <base>/programs/
are owned by bpfman and safe to delete when not backed by a DB row.

Severity: WARNING
Category: gc-orphan-pin`,
		kinds:   []OrphanKind{OrphanProgramDir, OrphanProgramDirUnk},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		actionFn: func(o FsOrphan) action.Action {
			return action.RemoveProgramDir{Path: o.Path}
		},
		describeFn: func(o FsOrphan) (string, string) {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
				fmt.Sprintf("remove program dir %s", o.Path)
		},
	},
	{
		name: "orphan-shared-map-pins",
		description: `Removes orphan shared map pin files under {bpffs}/shared/ that
have no corresponding entry in the shared_map_pins table. These pins
reference kernel maps that will be garbage-collected once no FD or pin
holds them. They indicate incomplete cleanup from a crash or database
wipe.

Severity: WARNING
Category: gc-orphan-pin`,
		kinds:   []OrphanKind{OrphanSharedMapPin},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		actionFn: func(o FsOrphan) action.Action {
			return action.RemoveSharedMapPin{Path: bpfman.MapPinPath(o.Path)}
		},
		describeFn: func(o FsOrphan) (string, string) {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
				fmt.Sprintf("remove shared map pin %s", o.Path)
		},
	},
	{
		name: "orphan-staging-dirs",
		description: `Removes orphan staging directories under <base>/.staging/.
Staging directories are transient scratch space used during atomic
publish operations. They are never referenced by DB rows and are
always safe to delete.

Severity: WARNING
Category: gc-orphan-pin`,
		kinds:   []OrphanKind{OrphanStagingDir},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		actionFn: func(o FsOrphan) action.Action {
			return action.RemoveStagingDir{Path: o.Path}
		},
		describeFn: func(o FsOrphan) (string, string) {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
				fmt.Sprintf("remove staging dir %s", o.Path)
		},
	},
}

// orphanRule builds a Rule from an orphanGCSpec. The shared
// evaluation logic iterates OrphanFsEntries, filters by kind set
// and liveness, and emits violations with removal actions.
func orphanRule(spec orphanGCSpec) Rule {
	kindSet := make(map[OrphanKind]bool, len(spec.kinds))
	for _, k := range spec.kinds {
		kindSet[k] = true
	}
	return Rule{
		Name:        spec.name,
		Description: spec.description,
		Eval: func(s *ObservedState) []Violation {
			var out []Violation
			for _, o := range s.OrphanFsEntries() {
				if !kindSet[o.Kind] {
					continue
				}
				if !spec.include(s, o) {
					continue
				}
				desc, opDesc := spec.describeFn(o)
				out = append(out, Violation{
					Severity:    SeverityWarning,
					Category:    "gc-orphan-pin",
					Description: desc,
					Op: &Operation{
						Description: opDesc,
						Actions:     []action.Action{spec.actionFn(o)},
					},
				})
			}
			return out
		},
	}
}

// PruneRule returns a GC rule that removes live orphan program pins.
//
// A live orphan is a program that bpfman originally loaded and pinned
// (evidenced by paths like /run/bpfman/fs/prog_<id>) but has since
// lost track of, typically because the database was wiped or recreated
// while the bpffs pins survived across restarts. The pin holds a
// kernel reference, keeping the program alive even though bpfman no
// longer manages it.
//
// Removing the pin releases bpfman's reference. The kernel reclaims
// the program when no other references (file descriptors, other pins,
// links) remain.
func PruneRule() Rule {
	return orphanRule(orphanGCSpec{
		name: "prune-live-orphans",
		description: `Removes live orphan program pins, link directories, and map
directories. A live orphan is a program that bpfman originally loaded
(pinned under bpfman's bpffs root) but no longer tracks in its
database. This typically occurs when the database is wiped or recreated
while bpffs pins survive across restarts.

Unlike orphan-program-artefacts (which only removes dead orphans),
this rule removes the pin even when the kernel program is alive.
Removing the pin releases bpfman's reference; the kernel reclaims the
program when no other references remain.

Severity: WARNING
Category: gc-orphan-pin`,
		kinds: []OrphanKind{OrphanProgPin, OrphanLinkDir, OrphanMapDir},
		include: func(s *ObservedState, o FsOrphan) bool {
			return o.ProgramID != 0 && s.KernelAlive(o.ProgramID)
		},
		actionFn: orphanPinAction,
		describeFn: func(o FsOrphan) (string, string) {
			return fmt.Sprintf("Live orphan %s: %s (kernel program %d alive)", o.Kind, o.Path, o.ProgramID),
				fmt.Sprintf("remove %s", o.Path)
		},
	})
}

// orphanPinAction maps an orphan's kind to the appropriate removal action.
func orphanPinAction(o FsOrphan) action.Action {
	switch o.Kind {
	case OrphanProgPin:
		return action.RemoveProgPin{Path: o.Path}
	case OrphanLinkDir:
		return action.RemoveLinkDir{Path: o.Path}
	case OrphanMapDir:
		return action.RemoveMapDir{Path: o.Path}
	default:
		panic(fmt.Sprintf("orphanPinAction: unexpected kind %s", o.Kind))
	}
}
