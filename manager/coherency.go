package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/interpreter"
)

// --------------------------------------------------------------------
// Core types used by doctor and GC consumers.
// --------------------------------------------------------------------

// Severity indicates the severity of a coherency finding.
type Severity int

const (
	SeverityOK Severity = iota
	SeverityWarning
	SeverityError
)

// String returns a human-readable label for the severity.
func (s Severity) String() string {
	switch s {
	case SeverityOK:
		return "OK"
	case SeverityWarning:
		return "WARNING"
	case SeverityError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// Finding describes a single coherency check result.
type Finding struct {
	Severity    Severity
	Category    string
	RuleName    string
	Description string
}

// DoctorReport contains the results of a coherency check.
type DoctorReport struct {
	Findings []Finding
}

// HasErrors returns true if any finding has error severity.
func (r DoctorReport) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// HasWarnings returns true if any finding has warning severity.
func (r DoctorReport) HasWarnings() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

// tcParent returns the TC parent handle for the given dispatcher type.
func tcParent(dt dispatcher.DispatcherType) uint32 {
	if dt == dispatcher.DispatcherTypeTCIngress {
		return 0xFFFFFFF1 // TC_H_CLSACT | TC_H_MIN_INGRESS
	}
	return 0xFFFFFFF3 // TC_H_CLSACT | TC_H_MIN_EGRESS
}

// --------------------------------------------------------------------
// Tuple types: correlated views across DB, kernel, and filesystem.
// Each field is nil when the object is absent in that source.
// --------------------------------------------------------------------

// ProgramState correlates a program across all three sources.
// Primary key: kernel program ID.
type ProgramState struct {
	KernelID uint32
	DB       *bpfman.ProgramSpec // nil = no DB record
	Kernel   bool                // true = kernel program alive
	PinPath  string              // derived from DB; empty if no DB record
	PinExist *bool               // nil = not checked; non-nil = stat result
}

// LinkState correlates a link across DB and kernel.
// Primary key: bpfman link ID.
type LinkState struct {
	DB        *bpfman.LinkSpec // nil = no DB record
	Kernel    bool             // true = kernel link alive
	Synthetic bool             // true = perf_event synthetic ID (no kernel link)
	PinExist  *bool            // nil = not checked
}

// DispatcherState correlates a dispatcher across all three sources.
// Primary key: (type, nsid, ifindex).
type DispatcherState struct {
	DB           *dispatcher.State // nil = no DB record
	KernelProg   bool              // true = dispatcher kernel program alive
	KernelLink   bool              // true = XDP link alive (irrelevant for TC)
	ProgPinExist *bool             // nil = not checked
	LinkPinExist *bool             // nil = not checked (XDP only)
	TCFilterOK   *bool             // nil = not checked (TC only)
	LinkCount    int               // number of extension links (-1 = unknown)
	RevDir       string            // computed revision directory path
	ProgPin      string            // computed prog pin path
}

// FsOrphan represents a filesystem entry with no matching DB record.
type FsOrphan struct {
	Path     string
	KernelID uint32 // parsed from name; 0 if not parseable
	Kind     string // "prog-pin", "link-dir", "map-dir", "dispatcher-dir", "dispatcher-link"
}

// Operation is a planned mutation. Rules emit operations; the
// executor applies them. This separates planning from doing.
type Operation struct {
	Description string
	Execute     func() error
}

// Violation is a coherency rule violation with an optional planned
// operation for GC to execute.
type Violation struct {
	Severity    Severity
	Category    string
	RuleName    string
	Description string
	Op          *Operation // nil = report only
}

// Finding returns the violation as a Finding for doctor output.
func (v Violation) Finding() Finding {
	return Finding{
		Severity:    v.Severity,
		Category:    v.Category,
		RuleName:    v.RuleName,
		Description: v.Description,
	}
}

// Rule is a declarative coherency check evaluated over an
// ObservedState snapshot.
type Rule struct {
	Name        string
	Description string // detailed explanation for 'doctor explain'
	Eval        func(s *ObservedState) []Violation
}

// --------------------------------------------------------------------
// ObservedState: the system snapshot with correlated views.
// --------------------------------------------------------------------

// ObservedState is a point-in-time snapshot of all three state
// sources with pre-built correlated views. Rules consume this;
// they never reach back into raw maps. All I/O happens during
// GatherState; view builders and rules are pure joins over facts.
type ObservedState struct {
	// DB facts.
	dbPrograms    map[uint32]bpfman.ProgramSpec
	dbLinks       []bpfman.LinkSpec
	dbDispatchers []dispatcher.State

	// Kernel facts.
	kernelProgs          map[uint32]bool
	kernelLinks          map[uint32]bool
	kernelProgEnumErrors int
	kernelLinkEnumErrors int

	// Filesystem facts: pin existence by path.
	fsPinExists map[string]bool

	// Filesystem facts: directory scans.
	// These are built during gather from bpffs directory listings.
	orphans []FsOrphan

	// Filesystem facts: dispatcher revision directory link counts.
	// Key is dispatcherKey(type, nsid, ifindex).
	fsDispatcherLinkCount map[string]int

	// Store-derived facts: dispatcher extension link counts.
	// Key is dispatcher kernel program ID.
	dbDispatcherExtCount map[uint32]int

	// Netlink-derived facts: TC filter existence.
	// Key is dispatcherKey(type, nsid, ifindex).
	tcFilterOK map[string]bool

	// Indexes for join operations.
	dbProgPins       map[string]bool
	dbProgIDs        map[uint32]bool
	dbDispatcherKeys map[string]bool

	// Runtime context (immutable after gather).
	root fs.Root

	// Mutation capability for GC operations only.
	// Not used during rule evaluation.
	deleteDispatcher func(dispType string, nsid uint64, ifindex uint32) error

	// Cached views (built lazily on first access, pure joins).
	programs    []ProgramState
	links       []LinkState
	dispatchers []DispatcherState
}

// GatherState builds an ObservedState by scanning all three sources.
// All I/O happens here; the returned state is a pure fact store.
func GatherState(ctx context.Context, store interpreter.Store, kernel interpreter.KernelOperations, root fs.Root) (*ObservedState, error) {
	s := &ObservedState{
		kernelProgs:           make(map[uint32]bool),
		kernelLinks:           make(map[uint32]bool),
		fsPinExists:           make(map[string]bool),
		fsDispatcherLinkCount: make(map[string]int),
		dbDispatcherExtCount:  make(map[uint32]int),
		tcFilterOK:            make(map[string]bool),
		dbProgPins:            make(map[string]bool),
		dbProgIDs:             make(map[uint32]bool),
		dbDispatcherKeys:      make(map[string]bool),
		root:                  root,
	}

	var err error

	// ----------------------------------------------------------------
	// Phase 1: DB facts
	// ----------------------------------------------------------------

	s.dbPrograms, err = store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list programs: %w", err)
	}

	s.dbLinks, err = store.ListLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}

	s.dbDispatchers, err = store.ListDispatchers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dispatchers: %w", err)
	}

	// Build DB indexes.
	for kernelID, prog := range s.dbPrograms {
		s.dbProgIDs[kernelID] = true
		if prog.Handles.PinPath != "" {
			s.dbProgPins[prog.Handles.PinPath] = true
		}
	}
	for _, d := range s.dbDispatchers {
		s.dbDispatcherKeys[dispatcherKey(d.Type, d.Nsid, d.Ifindex)] = true
	}

	// ----------------------------------------------------------------
	// Phase 2: Kernel facts
	// ----------------------------------------------------------------

	for kp, err := range kernel.Programs(ctx) {
		if err != nil {
			s.kernelProgEnumErrors++
			continue
		}
		s.kernelProgs[kp.ID] = true
	}

	for kl, err := range kernel.Links(ctx) {
		if err != nil {
			s.kernelLinkEnumErrors++
			continue
		}
		s.kernelLinks[kl.ID] = true
	}

	// ----------------------------------------------------------------
	// Phase 3: Store-derived facts (dispatcher extension counts)
	// ----------------------------------------------------------------

	for _, d := range s.dbDispatchers {
		if count, err := store.CountDispatcherLinks(ctx, d.KernelID); err == nil {
			s.dbDispatcherExtCount[d.KernelID] = count
		}
	}

	// ----------------------------------------------------------------
	// Phase 4: Netlink facts (TC filter checks)
	// ----------------------------------------------------------------

	for _, d := range s.dbDispatchers {
		if d.Type != dispatcher.DispatcherTypeTCIngress && d.Type != dispatcher.DispatcherTypeTCEgress {
			continue
		}
		if d.Priority == 0 {
			continue
		}
		key := dispatcherKey(d.Type, d.Nsid, d.Ifindex)
		parent := tcParent(d.Type)
		_, err := kernel.FindTCFilterHandle(ctx, int(d.Ifindex), parent, d.Priority)
		s.tcFilterOK[key] = (err == nil)
	}

	// ----------------------------------------------------------------
	// Phase 5: Filesystem facts - collect paths to stat
	// ----------------------------------------------------------------

	pathsToStat := make(map[string]struct{})

	// Program pin paths from DB.
	for _, prog := range s.dbPrograms {
		if prog.Handles.PinPath != "" {
			pathsToStat[prog.Handles.PinPath] = struct{}{}
		}
	}

	// Link pin paths from DB (non-synthetic only).
	for i := range s.dbLinks {
		link := &s.dbLinks[i]
		if link.PinPath != nil && !link.IsSynthetic() {
			pathsToStat[link.PinPath.String()] = struct{}{}
		}
	}

	// Dispatcher prog pins and XDP link pins.
	for _, d := range s.dbDispatchers {
		revDir := dispatcher.DispatcherRevisionDir(root.BPFFS().FS(), d.Type, d.Nsid, d.Ifindex, d.Revision)
		progPin := dispatcher.DispatcherProgPath(revDir)
		pathsToStat[progPin] = struct{}{}

		if d.Type == dispatcher.DispatcherTypeXDP {
			linkPin := dispatcher.DispatcherLinkPath(root.BPFFS().FS(), d.Type, d.Nsid, d.Ifindex)
			pathsToStat[linkPin] = struct{}{}
		}
	}

	// Stat all collected paths.
	for path := range pathsToStat {
		_, err := os.Stat(path)
		if err == nil {
			s.fsPinExists[path] = true
		} else if os.IsNotExist(err) {
			s.fsPinExists[path] = false
		}
		// Other errors (EPERM, EIO): path not in map = unknown.
	}

	// ----------------------------------------------------------------
	// Phase 6: Filesystem facts - directory scans for orphans
	// ----------------------------------------------------------------

	s.orphans = make([]FsOrphan, 0)

	// Scan dirs.FS for orphan prog_* pins.
	if entries, err := os.ReadDir(root.BPFFS().FS()); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "prog_") {
				continue
			}
			pinPath := filepath.Join(root.BPFFS().FS(), name)
			if s.dbProgPins[pinPath] {
				continue
			}
			var kernelID uint32
			if n, _ := fmt.Sscanf(name, "prog_%d", &kernelID); n == 1 {
				s.orphans = append(s.orphans, FsOrphan{Path: pinPath, KernelID: kernelID, Kind: "prog-pin"})
			}
		}
	}

	// Scan root.BPFFS().Links() for orphan link directories.
	if entries, err := os.ReadDir(root.BPFFS().Links()); err == nil {
		for _, entry := range entries {
			var progID uint32
			if n, _ := fmt.Sscanf(entry.Name(), "%d", &progID); n != 1 {
				continue
			}
			if s.dbProgIDs[progID] {
				continue
			}
			s.orphans = append(s.orphans, FsOrphan{
				Path:     filepath.Join(root.BPFFS().Links(), entry.Name()),
				KernelID: progID,
				Kind:     "link-dir",
			})
		}
	}

	// Scan root.BPFFS().Maps() for orphan map directories.
	if entries, err := os.ReadDir(root.BPFFS().Maps()); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			var progID uint32
			if n, _ := fmt.Sscanf(entry.Name(), "%d", &progID); n != 1 {
				continue
			}
			if s.dbProgIDs[progID] {
				continue
			}
			s.orphans = append(s.orphans, FsOrphan{
				Path:     filepath.Join(root.BPFFS().Maps(), entry.Name()),
				KernelID: progID,
				Kind:     "map-dir",
			})
		}
	}

	// Scan dispatcher type directories for orphan dispatchers and
	// count link_* files in non-orphan dispatcher revision dirs.
	dispTypes := []dispatcher.DispatcherType{
		dispatcher.DispatcherTypeXDP,
		dispatcher.DispatcherTypeTCIngress,
		dispatcher.DispatcherTypeTCEgress,
	}
	for _, dt := range dispTypes {
		typeDir := dispatcher.TypeDir(root.BPFFS().FS(), dt)
		entries, err := os.ReadDir(typeDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "dispatcher_") {
				continue
			}
			if entry.IsDir() {
				var nsid uint64
				var ifindex, revision uint32
				if n, _ := fmt.Sscanf(name, "dispatcher_%d_%d_%d", &nsid, &ifindex, &revision); n != 3 {
					continue
				}
				key := dispatcherKey(dt, nsid, ifindex)
				if !s.dbDispatcherKeys[key] {
					// Orphan dispatcher directory.
					s.orphans = append(s.orphans, FsOrphan{
						Path: filepath.Join(typeDir, name),
						Kind: "dispatcher-dir",
					})
				} else {
					// Non-orphan: count link_* files for consistency check.
					revDir := filepath.Join(typeDir, name)
					if revEntries, err := os.ReadDir(revDir); err == nil {
						count := 0
						for _, re := range revEntries {
							if strings.HasPrefix(re.Name(), "link_") {
								count++
							}
						}
						s.fsDispatcherLinkCount[key] = count
					}
				}
			} else if strings.HasSuffix(name, "_link") {
				var nsid uint64
				var ifindex uint32
				if n, _ := fmt.Sscanf(name, "dispatcher_%d_%d_link", &nsid, &ifindex); n != 2 {
					continue
				}
				if !s.dbDispatcherKeys[dispatcherKey(dt, nsid, ifindex)] {
					// Orphan dispatcher link pin.
					s.orphans = append(s.orphans, FsOrphan{
						Path: filepath.Join(typeDir, name),
						Kind: "dispatcher-link",
					})
				}
			}
		}
	}

	// ----------------------------------------------------------------
	// Phase 6a: Scan <base>/programs/ for orphan program dirs
	// ----------------------------------------------------------------

	if root.Valid() {
		rt := root.Runtime()
		programsPath := filepath.Join(root.Base(), "programs")
		if entries, err := os.ReadDir(programsPath); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				name := entry.Name()
				var kernelID uint32
				if n, _ := fmt.Sscanf(name, "%d", &kernelID); n == 1 {
					if !s.dbProgIDs[kernelID] {
						s.orphans = append(s.orphans, FsOrphan{
							Path:     filepath.Join(programsPath, name),
							KernelID: kernelID,
							Kind:     "program-dir",
						})
					}
				} else {
					s.orphans = append(s.orphans, FsOrphan{
						Path: filepath.Join(programsPath, name),
						Kind: "program-dir-unknown",
					})
				}
			}
		}

		// ----------------------------------------------------------------
		// Phase 6b: Scan <base>/.staging/ for orphan staging dirs
		// ----------------------------------------------------------------

		stagingPath := filepath.Join(root.Base(), ".staging")
		if entries, err := os.ReadDir(stagingPath); err == nil {
			for _, entry := range entries {
				s.orphans = append(s.orphans, FsOrphan{
					Path: filepath.Join(stagingPath, entry.Name()),
					Kind: "staging-dir",
				})
			}
		}
		_ = rt // used in GC rule closures via s.root
	}

	// ----------------------------------------------------------------
	// Wire up mutation capability for GC operations only.
	// ----------------------------------------------------------------

	s.deleteDispatcher = func(dispType string, nsid uint64, ifindex uint32) error {
		return store.DeleteDispatcher(ctx, dispType, nsid, ifindex)
	}

	return s, nil
}

// --------------------------------------------------------------------
// View builders: construct correlated tuples from raw facts.
// All joins happen here. Rules never touch raw maps.
// --------------------------------------------------------------------

// Programs returns one ProgramState per DB program, correlated with
// kernel and filesystem state. This is a pure join over gathered facts.
func (s *ObservedState) Programs() []ProgramState {
	if s.programs != nil {
		return s.programs
	}
	for id, prog := range s.dbPrograms {
		ps := ProgramState{
			KernelID: id,
			DB:       &prog,
			Kernel:   s.kernelProgs[id],
			PinPath:  prog.Handles.PinPath,
		}
		if prog.Handles.PinPath != "" {
			if exists, ok := s.fsPinExists[prog.Handles.PinPath]; ok {
				ps.PinExist = &exists
			}
			// Path not in map = stat failed with unknown error.
		}
		s.programs = append(s.programs, ps)
	}
	return s.programs
}

// Links returns one LinkState per DB link, correlated with kernel
// state and filesystem. This is a pure join over gathered facts.
func (s *ObservedState) Links() []LinkState {
	if s.links != nil {
		return s.links
	}
	for i := range s.dbLinks {
		link := &s.dbLinks[i]
		synthetic := link.IsSynthetic()
		inKernel := false
		// For non-synthetic links, ID is the kernel link ID
		if !synthetic {
			inKernel = s.kernelLinks[uint32(link.ID)]
		}
		ls := LinkState{
			DB:        link,
			Synthetic: synthetic,
			Kernel:    inKernel,
		}
		if link.PinPath != nil && !synthetic {
			if exists, found := s.fsPinExists[link.PinPath.String()]; found {
				ls.PinExist = &exists
			}
		}
		s.links = append(s.links, ls)
	}
	return s.links
}

// Dispatchers returns one DispatcherState per DB dispatcher,
// correlated with kernel, filesystem, and extension link counts.
// This is a pure join over gathered facts.
func (s *ObservedState) Dispatchers() []DispatcherState {
	if s.dispatchers != nil {
		return s.dispatchers
	}
	for _, d := range s.dbDispatchers {
		key := dispatcherKey(d.Type, d.Nsid, d.Ifindex)
		revDir := dispatcher.DispatcherRevisionDir(s.root.BPFFS().FS(), d.Type, d.Nsid, d.Ifindex, d.Revision)
		progPin := dispatcher.DispatcherProgPath(revDir)

		ds := DispatcherState{
			DB:         &d,
			KernelProg: s.kernelProgs[d.KernelID],
			RevDir:     revDir,
			ProgPin:    progPin,
			LinkCount:  -1,
		}

		// Prog pin existence from gathered facts.
		if exists, ok := s.fsPinExists[progPin]; ok {
			ds.ProgPinExist = &exists
		}

		// XDP link checks from gathered facts.
		if d.Type == dispatcher.DispatcherTypeXDP {
			ds.KernelLink = d.LinkID != 0 && s.kernelLinks[d.LinkID]
			linkPin := dispatcher.DispatcherLinkPath(s.root.BPFFS().FS(), d.Type, d.Nsid, d.Ifindex)
			if exists, ok := s.fsPinExists[linkPin]; ok {
				ds.LinkPinExist = &exists
			}
		}

		// TC filter check from gathered facts.
		if d.Type == dispatcher.DispatcherTypeTCIngress || d.Type == dispatcher.DispatcherTypeTCEgress {
			if d.Priority > 0 {
				if ok, found := s.tcFilterOK[key]; found {
					ds.TCFilterOK = &ok
				}
			}
		}

		// Extension link count from gathered facts.
		if count, found := s.dbDispatcherExtCount[d.KernelID]; found {
			ds.LinkCount = count
		}

		s.dispatchers = append(s.dispatchers, ds)
	}
	return s.dispatchers
}

// OrphanFsEntries returns filesystem entries under the bpffs tree
// that have no corresponding DB record. The list is pre-built during
// GatherState; this method is a pure accessor.
func (s *ObservedState) OrphanFsEntries() []FsOrphan {
	return s.orphans
}

// DispatcherFsLinkCount returns the count of link_* files in the
// dispatcher's revision directory. The count is pre-computed during
// GatherState; this method is a pure lookup. Returns -1 if unknown.
func (s *ObservedState) DispatcherFsLinkCount(ds DispatcherState) int {
	if ds.DB == nil {
		return -1
	}
	key := dispatcherKey(ds.DB.Type, ds.DB.Nsid, ds.DB.Ifindex)
	if count, ok := s.fsDispatcherLinkCount[key]; ok {
		return count
	}
	return -1
}

// KernelAlive reports whether a kernel program ID is alive.
func (s *ObservedState) KernelAlive(kernelID uint32) bool {
	return s.kernelProgs[kernelID]
}

// LiveOrphans returns the count of orphan program pins where the
// kernel program is still alive. These are programs that bpfman
// originally loaded (pinned under bpfman's bpffs root) but no longer
// tracks in its database, typically after a database wipe while pins
// survived. Standard GC leaves these untouched because removing the
// pin would unload a running program; use --prune to remove them.
func (s *ObservedState) LiveOrphans() int {
	count := 0
	for _, o := range s.orphans {
		if o.Kind == "prog-pin" && o.KernelID != 0 && s.kernelProgs[o.KernelID] {
			count++
		}
	}
	return count
}

// DeleteDispatcher delegates to the store to remove a dispatcher.
func (s *ObservedState) DeleteDispatcher(dispType string, nsid uint64, ifindex uint32) error {
	return s.deleteDispatcher(dispType, nsid, ifindex)
}

func dispatcherKey(dt dispatcher.DispatcherType, nsid uint64, ifindex uint32) string {
	return fmt.Sprintf("%s/%d/%d", dt, nsid, ifindex)
}

// --------------------------------------------------------------------
// Evaluate: uniform rule evaluation.
// --------------------------------------------------------------------

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

// --------------------------------------------------------------------
// Doctor rules: read-only coherency checks.
// Rules consume tuples. No raw map lookups. No joins.
// --------------------------------------------------------------------

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
				total := s.kernelProgEnumErrors + s.kernelLinkEnumErrors
				if total > 0 {
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "enumeration",
						Description: fmt.Sprintf("Kernel enumeration incomplete (%d program errors, %d link errors); results may be partial", s.kernelProgEnumErrors, s.kernelLinkEnumErrors),
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
							Description: fmt.Sprintf("Program %d in DB not found in kernel (pin: %s)", p.KernelID, p.PinPath),
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
							Description: fmt.Sprintf("Dispatcher %s nsid=%d ifindex=%d: program %d not found in kernel", d.DB.Type, d.DB.Nsid, d.DB.Ifindex, d.DB.KernelID),
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
							Description: fmt.Sprintf("Program %d: pin path missing: %s", p.KernelID, p.PinPath),
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
					if o.Kind == "prog-pin" && o.KernelID != 0 && s.KernelAlive(o.KernelID) {
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
					if o.Kind != "prog-pin" || o.KernelID == 0 {
						continue
					}
					if !s.KernelAlive(o.KernelID) {
						continue // not a live EBUSY risk
					}
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "kernel-vs-db",
						Description: fmt.Sprintf("Kernel program %d is pinned under %s but not tracked in DB; may cause EBUSY", o.KernelID, o.Path),
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
					if o.Kind != "program-dir" && o.Kind != "program-dir-unknown" {
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

// --------------------------------------------------------------------
// GC rules: detect stale state and plan mutations.
// Each violation carries an Operation the executor can apply.
// --------------------------------------------------------------------

// GCRules returns rules that detect and plan repairs for stale state.
func GCRules() []Rule {
	return []Rule{
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
					dd := d // capture
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-dispatcher",
						Description: fmt.Sprintf("Stale dispatcher %s nsid=%d ifindex=%d: no extensions, functionally dead", d.DB.Type, d.DB.Nsid, d.DB.Ifindex),
						Op: &Operation{
							Description: fmt.Sprintf("delete dispatcher %s/%d/%d and filesystem artefacts", d.DB.Type, d.DB.Nsid, d.DB.Ifindex),
							Execute: func() error {
								os.Remove(dd.ProgPin)
								os.RemoveAll(dd.RevDir)
								if dd.DB.Type == dispatcher.DispatcherTypeXDP {
									linkPin := dispatcher.DispatcherLinkPath(s.root.BPFFS().FS(), dd.DB.Type, dd.DB.Nsid, dd.DB.Ifindex)
									os.Remove(linkPin)
								}
								return s.DeleteDispatcher(string(dd.DB.Type), dd.DB.Nsid, dd.DB.Ifindex)
							},
						},
					})
				}
				return out
			},
		},
		// Orphan program pins, link directories, and map directories
		// with no DB record and no live kernel object.
		{
			Name: "orphan-program-artefacts",
			Description: `Removes orphan filesystem artefacts for programs that are no longer
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
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					if o.Kind != "prog-pin" && o.Kind != "link-dir" && o.Kind != "map-dir" {
						continue
					}
					if o.KernelID != 0 && s.KernelAlive(o.KernelID) {
						continue // kernel object alive; leave it
					}
					oo := o // capture
					isDir := o.Kind != "prog-pin"
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-orphan-pin",
						Description: fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
						Op: &Operation{
							Description: fmt.Sprintf("remove %s", o.Path),
							Execute: func() error {
								if isDir {
									return os.RemoveAll(oo.Path)
								}
								return os.Remove(oo.Path)
							},
						},
					})
				}
				return out
			},
		},
		// Orphan dispatcher directories and link pins with no
		// corresponding DB dispatcher.
		{
			Name: "orphan-dispatcher-artefacts",
			Description: `Removes orphan dispatcher filesystem artefacts that have no
corresponding database record. This includes:
  - dispatcher_* revision directories
  - dispatcher_*_link pin files

These artefacts indicate a dispatcher that was partially cleaned up
or created by a different bpfman instance. They waste filesystem
space and can cause confusion.

Severity: WARNING
Category: gc-orphan-pin`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					if o.Kind != "dispatcher-dir" && o.Kind != "dispatcher-link" {
						continue
					}
					oo := o
					isDir := o.Kind == "dispatcher-dir"
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-orphan-pin",
						Description: fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
						Op: &Operation{
							Description: fmt.Sprintf("remove %s", o.Path),
							Execute: func() error {
								if isDir {
									return os.RemoveAll(oo.Path)
								}
								return os.Remove(oo.Path)
							},
						},
					})
				}
				return out
			},
		},
		// Orphan program directories under <base>/programs/ with
		// no corresponding DB record.
		{
			Name: "orphan-program-dirs",
			Description: `Removes orphan program directories under <base>/programs/ that
have no corresponding database record. These directories contain
persisted bytecode and provenance from a previous load that was
either rolled back, crashed, or whose DB row was removed.

Both numeric (program-dir) and non-numeric (program-dir-unknown)
directory names are removed. All entries under <base>/programs/
are owned by bpfman and safe to delete when not backed by a DB row.

Severity: WARNING
Category: gc-orphan-pin`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					if o.Kind != "program-dir" && o.Kind != "program-dir-unknown" {
						continue
					}
					oo := o
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-orphan-pin",
						Description: fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
						Op: &Operation{
							Description: fmt.Sprintf("remove program dir %s", o.Path),
							Execute: func() error {
								if s.root.Valid() {
									return s.root.Runtime().RemoveProgram(oo.KernelID)
								}
								return os.RemoveAll(oo.Path)
							},
						},
					})
				}
				return out
			},
		},
		// Orphan staging directories under <base>/.staging/.
		{
			Name: "orphan-staging-dirs",
			Description: `Removes orphan staging directories under <base>/.staging/.
Staging directories are transient scratch space used during atomic
publish operations. They are never referenced by DB rows and are
always safe to delete.

Severity: WARNING
Category: gc-orphan-pin`,
			Eval: func(s *ObservedState) []Violation {
				var out []Violation
				for _, o := range s.OrphanFsEntries() {
					if o.Kind != "staging-dir" {
						continue
					}
					oo := o
					out = append(out, Violation{
						Severity:    SeverityWarning,
						Category:    "gc-orphan-pin",
						Description: fmt.Sprintf("Orphan %s: %s", o.Kind, o.Path),
						Op: &Operation{
							Description: fmt.Sprintf("remove staging dir %s", o.Path),
							Execute: func() error {
								return os.RemoveAll(oo.Path)
							},
						},
					})
				}
				return out
			},
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
	return Rule{
		Name: "prune-live-orphans",
		Description: `Removes live orphan program pins, link directories, and map
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
		Eval: func(s *ObservedState) []Violation {
			var out []Violation
			for _, o := range s.OrphanFsEntries() {
				if o.Kind != "prog-pin" && o.Kind != "link-dir" && o.Kind != "map-dir" {
					continue
				}
				if o.KernelID == 0 || !s.KernelAlive(o.KernelID) {
					continue // dead orphans handled by orphan-program-artefacts
				}
				oo := o // capture
				isDir := o.Kind != "prog-pin"
				out = append(out, Violation{
					Severity:    SeverityWarning,
					Category:    "gc-orphan-pin",
					Description: fmt.Sprintf("Live orphan %s: %s (kernel program %d alive)", o.Kind, o.Path, o.KernelID),
					Op: &Operation{
						Description: fmt.Sprintf("remove %s", o.Path),
						Execute: func() error {
							if isDir {
								return os.RemoveAll(oo.Path)
							}
							return os.Remove(oo.Path)
						},
					},
				})
			}
			return out
		},
	}
}

// --------------------------------------------------------------------
// Manager methods: Doctor and GC2 using the rule engine.
// --------------------------------------------------------------------

// Doctor gathers state and evaluates all coherency rules.
func (m *Manager) Doctor(ctx context.Context) (DoctorReport, error) {
	state, err := GatherState(ctx, m.store, m.kernel, m.root)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("gather state: %w", err)
	}

	violations := Evaluate(state, CoherencyRules())

	var report DoctorReport
	for _, v := range violations {
		report.Findings = append(report.Findings, v.Finding())
	}
	return report, nil
}

// CoherencyGC gathers state, evaluates GC rules, and executes
// planned operations. Returns the number of operations applied.
// This handles stale dispatchers and orphan filesystem artefacts.
// Store-level GC (structural cleanup) is handled separately by
// store.GC() called from Manager.GC().
func (m *Manager) CoherencyGC(ctx context.Context) (int, error) {
	state, err := GatherState(ctx, m.store, m.kernel, m.root)
	if err != nil {
		return 0, fmt.Errorf("gather state: %w", err)
	}

	violations := Evaluate(state, GCRules())

	applied := 0
	for _, v := range violations {
		if v.Op == nil {
			continue
		}
		if err := v.Op.Execute(); err != nil {
			m.logger.WarnContext(ctx, "gc operation failed",
				"op", v.Op.Description,
				"error", err)
			continue
		}
		m.logger.InfoContext(ctx, "gc operation applied", "op", v.Op.Description)
		applied++
	}
	return applied, nil
}
