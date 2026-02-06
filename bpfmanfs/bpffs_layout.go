package bpfmanfs

import (
	"path/filepath"
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
