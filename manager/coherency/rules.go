package coherency

import (
	"fmt"

	"github.com/frobware/go-bpfman/dispatcher"
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

// PruneRuleName is the name of the prune-live-orphans rule. The rule
// is destructive (removes pins for live kernel programs) and is not
// part of Rules(); callers opt in via the manager's --prune flag.
const PruneRuleName = "prune-live-orphans"

// Rules returns the always-on coherency rule registry. Each rule
// emits violations with an optional RepairIntent: nil for
// diagnostic-only findings, non-nil for findings GC can act on.
// Panics if any name appears twice; rule names are the only handle
// FindRule and RuleNames expose, so duplicates would silently shadow.
//
// PruneRule is intentionally excluded; it must be opted into and is
// surfaced separately via FindRule/RuleNames for introspection.
func Rules() []Rule {
	rules := append(diagnosticRules(), repairRules()...)
	seen := make(map[string]struct{}, len(rules)+1)
	for _, r := range rules {
		if _, dup := seen[r.Name]; dup {
			panic(fmt.Sprintf("coherency: duplicate rule name %q registered", r.Name))
		}
		seen[r.Name] = struct{}{}
	}
	if _, dup := seen[PruneRuleName]; dup {
		panic(fmt.Sprintf("coherency: duplicate rule name %q registered (collides with PruneRule)", PruneRuleName))
	}
	return rules
}

// allRulesIncludingPrune returns Rules() plus the opt-in PruneRule,
// for introspection callers (FindRule, RuleNames) that need the
// complete name set.
func allRulesIncludingPrune() []Rule {
	return append(Rules(), PruneRule())
}

// FindRule returns the rule with the given name, or nil if not found.
// Searches the full registry including the opt-in prune rule, so
// `bpfman audit explain prune-live-orphans` works.
func FindRule(name string) *Rule {
	for _, r := range allRulesIncludingPrune() {
		if r.Name == name {
			return &r
		}
	}
	return nil
}

// RuleNames returns the names of all rules including the opt-in
// prune rule, in registry order.
func RuleNames() []string {
	rules := allRulesIncludingPrune()
	names := make([]string, len(rules))
	for i, r := range rules {
		names[i] = r.Name
	}
	return names
}

// diagnosticRules returns rules that classify state without producing
// repair intents. They surface in audit output only.
func diagnosticRules() []Rule {
	return []Rule{
		// Warn if kernel enumeration was incomplete.
		{
			Name: "kernel-enumeration-incomplete",
			Description: `Bpfman could not enumerate every kernel BPF program or link the
kernel currently holds. Other audit findings that compare the
database against the kernel may therefore be incomplete: a finding
that looks like "DB row with no kernel program" may simply be a
program we failed to see. If this fires, investigate the cause
(reduced privileges, kernel-side limits) before acting on other
findings.`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				progErrors := s.obs.Meta.ProgramEnumErrors
				linkErrors := s.obs.Meta.LinkEnumErrors
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
			Description: `Every program tracked in the database must exist in the kernel. A
violation means the database references a program ID the kernel no
longer reports — typically because something else unloaded it, or
a previous unload was interrupted between kernel and database.
Operations targeting this program will fail.`,
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
			Description: `Every BPF link tracked in the database must have a kernel link
with the same ID. A violation means the link was detached outside
bpfman or the database survived a kernel-side teardown.

Synthetic perf_event link IDs are not enumerable by the kernel
iterator and are skipped.`,
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
			Description: `Every dispatcher in the database must have its multiplexing BPF
program loaded in the kernel. Dispatchers fan out a single hook
(XDP, TC ingress, TC egress) to multiple attached programs via
tail calls; if the dispatcher program is gone, every program
attached to that interface is broken — packets bypass the dispatch
chain entirely.`,
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
			Description: `Every XDP dispatcher must keep its kernel BPF link active. If the
link is gone, the dispatcher program is loaded but no longer
attached to the interface: traffic bypasses BPF entirely.`,
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
			Description: `Every TC dispatcher (ingress or egress) must have its netlink
filter installed. TC uses netlink filters rather than BPF links
to attach to interfaces.

Severity depends on whether the dispatcher has attached programs:
with extensions, a missing filter is an error — traffic should be
flowing through bpfman but isn't. With no extensions the
dispatcher is functionally dead, so a missing filter is only a
warning, and stale-dispatcher will offer to clean it up.`,
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
			Description: `Every program in the database must have its bpffs pin file. The
pin keeps the program addressable by path and holds a reference
that prevents the kernel from reclaiming it. A missing pin means
the program may disappear once other references drop.`,
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
			Description: `Every BPF link in the database must have its bpffs pin file. The
pin holds a reference that keeps the link attached; a missing pin
means the link may detach once other references drop. Synthetic
perf_event link IDs are skipped.`,
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
			Description: `Every dispatcher must have its program pin file on bpffs (under
{bpffs}/{type}/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher).
A missing pin means the dispatcher program may be reclaimed by
the kernel — which would break every program attached to that
interface.`,
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
			Description: `Every XDP dispatcher must have its link pin on bpffs (under
{bpffs}/xdp/dispatcher_{nsid}_{ifindex}_link). A missing pin means
the XDP attachment may be released, detaching the dispatcher from
the interface.`,
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
			Description: `Reports filesystem entries under bpfman's bpffs root that have
no matching database row: program pins (dead programs only),
link directories, map directories, dispatcher revision
directories, and dispatcher link pins. These are usually
leftovers from a crash or interrupted operation. Live program
pins are reported separately by kernel-program-pinned-but-not-in-db,
which carries EBUSY risk context.`,
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
			Description: `Reports kernel BPF programs that are still alive, pinned under
bpfman's bpffs root, but not tracked in the database. Usually
programs bpfman loaded before its database was wiped or recreated.

EBUSY risk: if such a program occupies an XDP or TC hook on an
interface, future attaches to that interface will fail with EBUSY
because the hook is already taken.

audit --repair will NOT remove these — removing the pin would
unload a running program. Use --repair --prune (which kills the
program) or remove the pin manually with rm /run/bpfman/fs/prog_XXXXX.`,
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
		// DB dispatcher link count must match the filesystem link count.
		{
			Name: "dispatcher-link-count",
			Description: `For each dispatcher, the number of extension links recorded in
the database must match the number of link_* files under its
revision directory on bpffs. A mismatch usually means a previous
attach or detach completed in one place but failed in the other.`,
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

// repairRules returns rules whose violations carry a RepairIntent.
// Audit surfaces them as warnings with the intent's Describe text;
// GC executes the lowered actions.
func repairRules() []Rule {
	rules := []Rule{
		// Dispatchers with zero extension links and missing attachment
		// mechanism (prog pin or TC filter) are functionally dead.
		{
			Name: "stale-dispatcher",
			Description: `Reports dispatchers that have nothing to do: zero attached
programs, plus a missing attachment mechanism. For XDP that means
the program pin is gone or the kernel BPF link has been detached;
for TC it means the netlink filter is not installed.

Such dispatchers serve no purpose — they have no programs to
multiplex and cannot receive traffic. audit --repair removes the
database row and any remaining filesystem artefacts.`,
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
					} else if d.DB.Type == dispatcher.DispatcherTypeXDP && d.DB.LinkID != 0 && !d.KernelLink {
						stale = true // outer link detached, post-detach cleanup left residue
					}
					if !stale {
						continue
					}
					var intent RepairIntent
					if d.DB.Type == dispatcher.DispatcherTypeXDP {
						intent = StaleXDPDispatcher{
							Nsid:    d.DB.Nsid,
							Ifindex: d.DB.Ifindex,
							ProgPin: d.ProgPin,
							RevDir:  d.RevDir,
							LinkPin: d.LinkPin,
						}
					} else {
						intent = StaleTCDispatcher{
							Type:    d.DB.Type,
							Nsid:    d.DB.Nsid,
							Ifindex: d.DB.Ifindex,
							ProgPin: d.ProgPin,
							RevDir:  d.RevDir,
						}
					}
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-dispatcher",
						Description: fmt.Sprintf("Stale dispatcher %s nsid=%d ifindex=%d: no extensions, functionally dead", d.DB.Type, d.DB.Nsid, d.DB.Ifindex),
						Intent:      intent,
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
// kind and liveness, and emits violations carrying a
// RemoveOrphanArtefact intent. The action-level lowering lives on
// the intent, not on the spec.
type orphanGCSpec struct {
	name        string
	description string
	kinds       []OrphanKind
	include     func(*ObservedState, FsOrphan) bool
	describeFn  func(FsOrphan) string
}

// orphanGCSpecs defines the four orphan GC rules. Each is
// distinguished by which orphan kinds it handles, its liveness
// filter, and how it maps an orphan to a removal action.
var orphanGCSpecs = []orphanGCSpec{
	{
		name: "orphan-program-artefacts",
		description: `Reports filesystem artefacts (program pins, link directories,
map directories) for programs the kernel no longer holds. They
have no database row and no live kernel object — only consumed
disk space. audit --repair removes them.

Live program pins are skipped here — handled by
kernel-program-pinned-but-not-in-db (or --prune for forced
removal).`,
		kinds: []OrphanKind{OrphanProgPin, OrphanLinkDir, OrphanMapDir},
		include: func(s *ObservedState, o FsOrphan) bool {
			return o.ProgramID == 0 || !s.KernelAlive(o.ProgramID)
		},
		describeFn: func(o FsOrphan) string {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path)
		},
	},
	{
		name: "orphan-dispatcher-artefacts",
		description: `Reports dispatcher revision directories and dispatcher link pins
under bpfman's bpffs root with no matching database row. Usually
leftovers from a partially completed teardown or a previous
bpfman instance. audit --repair removes them.`,
		kinds:   []OrphanKind{OrphanDispatcherDir, OrphanDispatcherLink},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		describeFn: func(o FsOrphan) string {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path)
		},
	},
	{
		name: "orphan-program-dirs",
		description: `Reports program directories under <base>/programs/ with no
matching database row. These hold persisted bytecode and
provenance from a load that was rolled back, crashed, or whose
database row was later removed. Everything under <base>/programs/
is owned by bpfman, so audit --repair removes the directory
whether the name is a numeric program ID or anything else.`,
		kinds:   []OrphanKind{OrphanProgramDir, OrphanProgramDirUnk},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		describeFn: func(o FsOrphan) string {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path)
		},
	},
	{
		name: "orphan-shared-map-pins",
		description: `Reports shared map pin files under {bpffs}/shared/ with no
matching shared_map_pins row. Usually leftovers from a crash or
a database wipe. audit --repair removes the pins; the kernel
reclaims the underlying map once nothing else holds a reference.`,
		kinds:   []OrphanKind{OrphanSharedMapPin},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		describeFn: func(o FsOrphan) string {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path)
		},
	},
	{
		name: "orphan-staging-dirs",
		description: `Reports leftover staging directories under <base>/.staging/.
Staging is transient scratch space used during atomic publish
operations; nothing under it is referenced by the database, so
audit --repair always removes them.`,
		kinds:   []OrphanKind{OrphanStagingDir},
		include: func(_ *ObservedState, _ FsOrphan) bool { return true },
		describeFn: func(o FsOrphan) string {
			return fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path)
		},
	},
}

// orphanRule builds a Rule from an orphanGCSpec. Each emitted
// violation carries a RemoveOrphanArtefact intent; the action-level
// lowering lives on the intent, not on the spec.
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
				out = append(out, Violation{
					Severity:    SeverityWarning,
					Category:    "gc-orphan-pin",
					Description: spec.describeFn(o),
					Intent:      RemoveOrphanArtefact{Kind: o.Kind, Path: o.Path},
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
		description: `Removes program pins, link directories, and map directories
that are pinned under bpfman's bpffs root and still alive in the
kernel, but not tracked in bpfman's database. Removing the pin
releases bpfman's reference; the kernel reclaims the program
once no other references remain.

This rule is destructive: it can unload programs that are
currently routing traffic. It only runs when --prune is passed
explicitly. Use it when you have decided the leftover state must
go.`,
		kinds: []OrphanKind{OrphanProgPin, OrphanLinkDir, OrphanMapDir},
		include: func(s *ObservedState, o FsOrphan) bool {
			return o.ProgramID != 0 && s.KernelAlive(o.ProgramID)
		},
		describeFn: func(o FsOrphan) string {
			return fmt.Sprintf("Live orphan %s: %s (kernel program %d alive)", o.Kind, o.Path, o.ProgramID)
		},
	})
}
