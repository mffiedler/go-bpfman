package coherency

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
)

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

// ProgramState correlates a program across all three sources.
// Primary key: kernel program ID.
type ProgramState struct {
	KernelID uint32
	DB       *bpfman.ProgramRecord // nil = no DB record
	Kernel   bool                  // true = kernel program alive
	PinPath  string                // derived from DB; empty if no DB record
	PinExist *bool                 // nil = not checked; non-nil = stat result
}

// LinkState correlates a link across DB and kernel.
// Primary key: bpfman link ID.
type LinkState struct {
	DB        *bpfman.LinkRecord // nil = no DB record
	Kernel    bool               // true = kernel link alive
	Synthetic bool               // true = perf_event synthetic ID (no kernel link)
	PinExist  *bool              // nil = not checked
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

// OrphanKind identifies the type of orphaned filesystem artefact.
type OrphanKind string

const (
	OrphanProgPin        OrphanKind = "prog-pin"
	OrphanLinkDir        OrphanKind = "link-dir"
	OrphanMapDir         OrphanKind = "map-dir"
	OrphanDispatcherDir  OrphanKind = "dispatcher-dir"
	OrphanDispatcherLink OrphanKind = "dispatcher-link"
	OrphanProgramDir     OrphanKind = "program-dir"
	OrphanProgramDirUnk  OrphanKind = "program-dir-unknown"
	OrphanStagingDir     OrphanKind = "staging-dir"
)

// FsOrphan represents a filesystem entry with no matching DB record.
type FsOrphan struct {
	Path     string
	KernelID uint32     // parsed from name; 0 if not parseable
	Kind     OrphanKind // type of orphaned artefact
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
