package coherency

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
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

// AuditReport carries the raw violations produced by a coherency
// check. Violations preserve their RepairIntent so callers can render
// planned actions or pass them to the GC executor; use Findings() for
// the projected diagnostic view.
type AuditReport struct {
	Violations []Violation
}

// Findings returns the report's violations projected to Finding
// values for display.
func (r AuditReport) Findings() []Finding {
	out := make([]Finding, len(r.Violations))
	for i, v := range r.Violations {
		out[i] = v.Finding()
	}
	return out
}

// HasErrors returns true if any violation has error severity.
func (r AuditReport) HasErrors() bool {
	for _, v := range r.Violations {
		if v.Severity == SeverityError {
			return true
		}
	}
	return false
}

// HasWarnings returns true if any violation has warning severity.
func (r AuditReport) HasWarnings() bool {
	for _, v := range r.Violations {
		if v.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

// ProgramState correlates a program across all three sources.
// Primary key: kernel program ID.
type ProgramState struct {
	ProgramID kernel.ProgramID
	DB        *bpfman.ProgramRecord // nil = no DB record
	Kernel    bool                  // true = kernel program alive
	PinPath   string                // derived from DB; empty if no DB record
	PinExist  *bool                 // nil = not checked; non-nil = stat result
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
	DB           *dispatcher.State       // nil = no DB record
	KernelProg   bool                    // true = dispatcher kernel program alive
	KernelLink   bool                    // true = XDP link alive (irrelevant for TC)
	ProgPinExist *bool                   // nil = not checked
	LinkPinExist *bool                   // nil = not checked (XDP only)
	TCFilterOK   *bool                   // nil = not checked (TC only)
	LinkCount    int                     // number of extension links (-1 = unknown)
	RevDir       bpfman.DispatcherRevDir // computed revision directory path
	ProgPin      bpfman.ProgPinPath      // computed prog pin path
	LinkPin      bpfman.LinkPath         // computed link pin path (XDP only; empty for TC)
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
	OrphanSharedMapPin   OrphanKind = "shared-map-pin"
)

// FsOrphan represents a filesystem entry with no matching DB record.
type FsOrphan struct {
	Path      string
	ProgramID kernel.ProgramID // parsed from name; 0 if not parseable
	Kind      OrphanKind       // type of orphaned artefact
}

// Violation is a coherency rule result. A nil Intent means
// diagnostic-only (audit-style finding, no automatic repair); a
// non-nil Intent describes the cleanup the GC interpreter would
// perform if the violation is acted on.
type Violation struct {
	Severity    Severity
	Category    string
	RuleName    string
	Description string
	Intent      RepairIntent // nil = report only
}

// Finding returns the violation as a Finding for audit output.
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
	Description string // detailed explanation for 'audit explain'
	Eval        func(s *ObservedState) []Violation
}
