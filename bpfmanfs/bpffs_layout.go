package bpfmanfs

import (
	"path/filepath"

	"github.com/frobware/go-bpfman/bpffs"
)

// BPFFS provides access to bpfman's bpffs layout conventions.
// Fields are unexported; obtain via Root.BPFFS().
type BPFFS struct {
	root Root
}

// Valid reports whether the BPFFS was obtained from a valid Root.
func (b BPFFS) Valid() bool {
	return b.root.Valid()
}

// FS returns the bpffs mount point path.
func (b BPFFS) FS() string {
	return filepath.Join(b.root.base, "fs")
}

// XDP returns the XDP dispatcher pins directory.
func (b BPFFS) XDP() string {
	return filepath.Join(b.root.base, "fs", "xdp")
}

// TCIngress returns the TC ingress dispatcher pins directory.
func (b BPFFS) TCIngress() string {
	return filepath.Join(b.root.base, "fs", "tc-ingress")
}

// TCEgress returns the TC egress dispatcher pins directory.
func (b BPFFS) TCEgress() string {
	return filepath.Join(b.root.base, "fs", "tc-egress")
}

// Maps returns the map pins directory.
func (b BPFFS) Maps() string {
	return filepath.Join(b.root.base, "fs", "maps")
}

// Links returns the link pins directory.
func (b BPFFS) Links() string {
	return filepath.Join(b.root.base, "fs", "links")
}

// ProgPinPath returns the pin path for a program.
// Format: {base}/fs/prog_{id}
func (b BPFFS) ProgPinPath(kernelID uint32) string {
	return filepath.Join(b.root.base, "fs", "prog_"+uitoa(kernelID))
}

// MapPinDir returns the directory for a program's map pins.
// Format: {base}/fs/maps/{program_id}/
func (b BPFFS) MapPinDir(programID uint32) string {
	return filepath.Join(b.root.base, "fs", "maps", uitoa(programID))
}

// LinkPinDir returns the directory for a program's link pins.
// Format: {base}/fs/links/{program_id}/
func (b BPFFS) LinkPinDir(programID uint32) string {
	return filepath.Join(b.root.base, "fs", "links", uitoa(programID))
}

// ScannerDirs returns a bpffs.ScannerDirs for use with bpffs.Scanner.
func (b BPFFS) ScannerDirs() bpffs.ScannerDirs {
	return bpffs.ScannerDirs{
		FS:        b.FS(),
		XDP:       b.XDP(),
		TCIngress: b.TCIngress(),
		TCEgress:  b.TCEgress(),
		Maps:      b.Maps(),
		Links:     b.Links(),
	}
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
