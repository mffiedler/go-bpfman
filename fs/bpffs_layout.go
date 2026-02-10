package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
)

// BPFFS provides access to bpfman's bpffs path conventions.
// Fields are unexported; obtain via Layout.BPFFS().
type BPFFS struct {
	layout Layout
}

// Valid reports whether the BPFFS was obtained from a valid Layout.
func (b BPFFS) Valid() bool {
	return b.layout.Valid()
}

// mustValid panics if b was not obtained from Layout.BPFFS().
func (b BPFFS) mustValid() {
	if !b.Valid() {
		panic("fs: zero BPFFS used; obtain via Layout.BPFFS()")
	}
}

// Internal path accessors used by Scanner.

func (b BPFFS) mountPoint() string {
	return filepath.Join(b.layout.base, "fs")
}

func (b BPFFS) xdpDir() string {
	return filepath.Join(b.layout.base, "fs", "xdp")
}

func (b BPFFS) tcIngressDir() string {
	return filepath.Join(b.layout.base, "fs", "tc-ingress")
}

func (b BPFFS) tcEgressDir() string {
	return filepath.Join(b.layout.base, "fs", "tc-egress")
}

func (b BPFFS) mapsDir() string {
	return filepath.Join(b.layout.base, "fs", "maps")
}

func (b BPFFS) linksDir() string {
	return filepath.Join(b.layout.base, "fs", "links")
}

// MountPoint returns the bpffs mount point path.
func (b BPFFS) MountPoint() string {
	b.mustValid()
	return b.mountPoint()
}

// XDP returns the XDP dispatcher pins directory.
func (b BPFFS) XDP() string {
	b.mustValid()
	return b.xdpDir()
}

// TCIngress returns the TC ingress dispatcher pins directory.
func (b BPFFS) TCIngress() string {
	b.mustValid()
	return b.tcIngressDir()
}

// TCEgress returns the TC egress dispatcher pins directory.
func (b BPFFS) TCEgress() string {
	b.mustValid()
	return b.tcEgressDir()
}

// Maps returns the map pins directory.
func (b BPFFS) Maps() string {
	b.mustValid()
	return b.mapsDir()
}

// Links returns the link pins directory.
func (b BPFFS) Links() string {
	b.mustValid()
	return b.linksDir()
}

// ProgPinPath returns the pin path for a program.
// Format: {base}/fs/prog_{id}
func (b BPFFS) ProgPinPath(kernelID kernel.ProgramID) string {
	b.mustValid()
	return filepath.Join(b.mountPoint(), "prog_"+strconv.FormatUint(uint64(kernelID), 10))
}

// MapPinDir returns the directory for a program's map pins.
// Format: {base}/fs/maps/{program_id}/
func (b BPFFS) MapPinDir(programID kernel.ProgramID) string {
	b.mustValid()
	return filepath.Join(b.mapsDir(), strconv.FormatUint(uint64(programID), 10))
}

// LinkPinDir returns the directory for a program's link pins.
// Format: {base}/fs/links/{program_id}/
func (b BPFFS) LinkPinDir(programID kernel.ProgramID) string {
	b.mustValid()
	return filepath.Join(b.linksDir(), strconv.FormatUint(uint64(programID), 10))
}

// LinkPinPath returns the pin path for a specific link.
// Format: {base}/fs/links/{program_id}/{link_name}
func (b BPFFS) LinkPinPath(programID kernel.ProgramID, linkName string) string {
	b.mustValid()
	return filepath.Join(b.linksDir(), strconv.FormatUint(uint64(programID), 10), linkName)
}

// MapPinPath returns the pin path for a specific map.
// Format: {base}/fs/maps/{program_id}/{map_name}
func (b BPFFS) MapPinPath(programID kernel.ProgramID, mapName string) string {
	b.mustValid()
	return filepath.Join(b.mapsDir(), strconv.FormatUint(uint64(programID), 10), mapName)
}

// Scanner returns a new Scanner for reading bpfman's bpffs layout.
func (b BPFFS) Scanner() *Scanner {
	return NewScanner(b)
}

// --------------------------------------------------------------------
// Dispatcher path methods
// --------------------------------------------------------------------

// dispatcherTypeDir returns the base directory for a dispatcher type.
func (b BPFFS) dispatcherTypeDir(dispType dispatcher.DispatcherType) string {
	return filepath.Join(b.mountPoint(), string(dispType))
}

// DispatcherLinkPath returns the stable path for the dispatcher link.
// This path remains constant across revisions, enabling atomic updates.
//
// Format: {bpffs}/{type}/dispatcher_{nsid}_{ifindex}_link
func (b BPFFS) DispatcherLinkPath(dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32) string {
	b.mustValid()
	return filepath.Join(
		b.dispatcherTypeDir(dispType),
		fmt.Sprintf("dispatcher_%d_%d_link", nsid, ifindex),
	)
}

// DispatcherRevisionDir returns the directory for a specific dispatcher revision.
// Each revision contains the dispatcher program and extension links.
//
// Format: {bpffs}/{type}/dispatcher_{nsid}_{ifindex}_{revision}
func (b BPFFS) DispatcherRevisionDir(dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32, revision uint32) string {
	b.mustValid()
	return filepath.Join(
		b.dispatcherTypeDir(dispType),
		fmt.Sprintf("dispatcher_%d_%d_%d", nsid, ifindex, revision),
	)
}

// DispatcherProgPath returns the path for the dispatcher program within a revision.
//
// Format: {bpffs}/{type}/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher
func (b BPFFS) DispatcherProgPath(dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32, revision uint32) string {
	b.mustValid()
	return filepath.Join(b.DispatcherRevisionDir(dispType, nsid, ifindex, revision), "dispatcher")
}

// ExtensionLinkPath returns the path for an extension link within a dispatcher revision.
// Each extension is attached to a dispatcher slot identified by position (0-9).
//
// Format: {bpffs}/{type}/dispatcher_{nsid}_{ifindex}_{revision}/link_{position}
func (b BPFFS) ExtensionLinkPath(dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32, revision uint32, position int) string {
	b.mustValid()
	return filepath.Join(b.DispatcherRevisionDir(dispType, nsid, ifindex, revision), fmt.Sprintf("link_%d", position))
}

// TCXLinkPath returns the path for a TCX link pin.
//
// Format: {bpffs}/tcx-{direction}/link_{nsid}_{ifindex}_{programID}
func (b BPFFS) TCXLinkPath(direction string, nsid uint64, ifindex uint32, programID kernel.ProgramID) string {
	b.mustValid()
	return filepath.Join(
		b.mountPoint(),
		fmt.Sprintf("tcx-%s", direction),
		fmt.Sprintf("link_%d_%d_%d", nsid, ifindex, programID),
	)
}

// --------------------------------------------------------------------
// I/O operations with path safety
// --------------------------------------------------------------------

// EnsureMapsDir creates the maps directory for a program if it doesn't exist.
// Format: {base}/fs/maps/{program_id}/
func (b BPFFS) EnsureMapsDir(kernelID kernel.ProgramID) error {
	b.mustValid()
	dir := b.MapPinDir(kernelID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &PathError{Op: "ensure_maps_dir", Path: dir, Err: err}
	}
	return nil
}

// SafeRemove removes a single file (e.g., a pin) from bpffs.
// Returns nil if the file does not exist.
// Returns an error if the path is outside the bpffs mount point.
//
// Both paths are cleaned before comparison to normalise "..", ".", and
// redundant separators.
func (b BPFFS) SafeRemove(path string) error {
	b.mustValid()
	cleanParent := filepath.Clean(b.mountPoint())
	cleanPath := filepath.Clean(path)

	rel, err := filepath.Rel(cleanParent, cleanPath)
	if err != nil {
		return ErrOutsideLayout{Parent: cleanParent, Target: cleanPath}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrOutsideLayout{Parent: cleanParent, Target: cleanPath}
	}
	if err := os.Remove(cleanPath); err != nil && !os.IsNotExist(err) {
		return &PathError{Op: "remove", Path: cleanPath, Err: err}
	}
	return nil
}

// SafeRemoveAll removes a directory and its contents from bpffs.
// Returns nil if the directory does not exist.
// Returns an error if the path is outside the bpffs mount point.
func (b BPFFS) SafeRemoveAll(path string) error {
	b.mustValid()
	return safeRemoveAll(b.mountPoint(), path)
}
