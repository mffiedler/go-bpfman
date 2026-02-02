package outcome

// This file contains operation-specific detail types used in Step.Details.
// All types must be JSON-serialisable.

// ProgramDetails contains details for kernel.load_program and kernel.unload_program steps.
//
// Target = program name (e.g., "xdp_pass")
//
// For kernel.unload_program: Details includes BOTH paths because
// UnloadProgram(progPinPath, mapsDir) removes both in a single call.
type ProgramDetails struct {
	KernelID uint32 `json:"kernel_id,omitempty"`
	PinPath  string `json:"pin_path,omitempty"`

	// MapsDirPath is populated for kernel.unload_program steps only.
	// For kernel.load_program, maps dir path is derived/implicit.
	MapsDirPath string `json:"maps_dir_path,omitempty"`
}

// LinkDetails contains details for attach and detach_link steps.
//
// Target = link ID (decimal string) for display/stability
//
// REQUIRED: For StepKindKernelDetachLink, PinPath must be non-empty
// when known. This ensures the step is actionable for manual cleanup.
type LinkDetails struct {
	LinkID       uint32 `json:"link_id,omitempty"`
	PinPath      string `json:"pin_path,omitempty"`
	ProgramID    uint32 `json:"program_id,omitempty"`
	Interface    string `json:"interface,omitempty"`
	Tracepoint   string `json:"tracepoint,omitempty"`
	Function     string `json:"function,omitempty"`
	DispatcherID uint32 `json:"dispatcher_id,omitempty"`
}

// DispatcherDetails contains details for dispatcher create/cleanup steps.
//
// Target = dispatcher identifier (e.g., "eth0:ingress")
type DispatcherDetails struct {
	DispatcherID uint32 `json:"dispatcher_id,omitempty"`
	Interface    string `json:"interface,omitempty"`
	Direction    string `json:"direction,omitempty"`
}

// GCPhaseDetails contains details for store.gc_* steps (GC Phase 1).
//
// Target = "store"
type GCPhaseDetails struct {
	Removed int `json:"removed"`
}

// OrphanDetails contains details for gc.remove_orphan steps (GC Phase 2).
//
// Target = resource identifier (kernel ID, pin path, etc.)
type OrphanDetails struct {
	// Category matches rule engine categories: "gc-dispatcher", "gc-orphan-pin"
	Category string `json:"category"`
	KernelID uint32 `json:"kernel_id,omitempty"`
	PinPath  string `json:"pin_path,omitempty"`
}

// ImageDetails contains details for image.pull and image.discover steps.
//
// Target = image URL or bytecode path
type ImageDetails struct {
	URL        string `json:"url,omitempty"`
	Digest     string `json:"digest,omitempty"`
	ObjectPath string `json:"object_path,omitempty"`
}

// TCFilterDetails contains details for kernel.detach_tc_filter steps.
//
// Target = <ifindex>:<parent>:<priority>:<handle>
type TCFilterDetails struct {
	Ifindex  int    `json:"ifindex,omitempty"`
	Parent   uint32 `json:"parent,omitempty"`
	Priority uint16 `json:"priority,omitempty"`
	Handle   uint32 `json:"handle,omitempty"`
}
