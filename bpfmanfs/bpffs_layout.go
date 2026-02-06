package bpfmanfs

import (
	"fmt"
	"path/filepath"

	"github.com/frobware/go-bpfman/dispatcher"
)

// BPFFS provides access to bpfman's bpffs path conventions.
// Fields are unexported; obtain via Root.BPFFS().
type BPFFS struct {
	root Root
	// dirs is set only for test scanners constructed via NewScannerFromDirs.
	dirs *ScannerDirs
}

// Valid reports whether the BPFFS was obtained from a valid Root.
func (b BPFFS) Valid() bool {
	return b.root.Valid() || b.dirs != nil
}

// mustValid panics if b was not obtained from Root.BPFFS().
func (b BPFFS) mustValid() {
	if !b.Valid() {
		panic("bpfmanfs: zero BPFFS used; obtain via Root.BPFFS()")
	}
}

// Internal path accessors used by Scanner. These check for dirs override first.

func (b BPFFS) mountPoint() string {
	if b.dirs != nil {
		return b.dirs.FS
	}
	return filepath.Join(b.root.base, "fs")
}

func (b BPFFS) xdpDir() string {
	if b.dirs != nil {
		return b.dirs.XDP
	}
	return filepath.Join(b.root.base, "fs", "xdp")
}

func (b BPFFS) tcIngressDir() string {
	if b.dirs != nil {
		return b.dirs.TCIngress
	}
	return filepath.Join(b.root.base, "fs", "tc-ingress")
}

func (b BPFFS) tcEgressDir() string {
	if b.dirs != nil {
		return b.dirs.TCEgress
	}
	return filepath.Join(b.root.base, "fs", "tc-egress")
}

func (b BPFFS) mapsDir() string {
	if b.dirs != nil {
		return b.dirs.Maps
	}
	return filepath.Join(b.root.base, "fs", "maps")
}

func (b BPFFS) linksDir() string {
	if b.dirs != nil {
		return b.dirs.Links
	}
	return filepath.Join(b.root.base, "fs", "links")
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
func (b BPFFS) ProgPinPath(kernelID uint32) string {
	b.mustValid()
	return filepath.Join(b.mountPoint(), "prog_"+uitoa(kernelID))
}

// MapPinDir returns the directory for a program's map pins.
// Format: {base}/fs/maps/{program_id}/
func (b BPFFS) MapPinDir(programID uint32) string {
	b.mustValid()
	return filepath.Join(b.mapsDir(), uitoa(programID))
}

// LinkPinDir returns the directory for a program's link pins.
// Format: {base}/fs/links/{program_id}/
func (b BPFFS) LinkPinDir(programID uint32) string {
	b.mustValid()
	return filepath.Join(b.linksDir(), uitoa(programID))
}

// Scanner returns a new Scanner for reading bpfman's bpffs layout.
func (b BPFFS) Scanner() *Scanner {
	return NewScanner(b)
}

// uitoa converts uint32 to string without importing strconv.
func uitoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
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
func (b BPFFS) TCXLinkPath(direction string, nsid uint64, ifindex uint32, programID uint32) string {
	b.mustValid()
	return filepath.Join(
		b.mountPoint(),
		fmt.Sprintf("tcx-%s", direction),
		fmt.Sprintf("link_%d_%d_%d", nsid, ifindex, programID),
	)
}
