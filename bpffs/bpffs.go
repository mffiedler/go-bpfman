// Package bpffs provides functions to check and mount the BPF
// filesystem.
package bpffs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	// DefaultMountInfoPath is the path to the mountinfo file.
	DefaultMountInfoPath = "/proc/self/mountinfo"

	// defaultScanMaxLineLen is the maximum line length for
	// scanning mountinfo. Some nodes/runtimes can produce long
	// lines; this prevents ErrTooLong.
	defaultScanMaxLineLen = 1024 * 1024
)

// MountPoint represents a bpffs mount point path.
// This is a newtype to prevent accidentally passing arbitrary strings
// where a bpffs mount point is expected.
type MountPoint string

// String returns the path as a string.
func (m MountPoint) String() string { return string(m) }

// LinkPath represents a pinned link path within a bpffs.
// This is a newtype to prevent accidentally passing arbitrary strings
// where a validated link pin path is expected.
type LinkPath string

// String returns the path as a string.
func (p LinkPath) String() string { return string(p) }

// NewLinkPath creates a *LinkPath from a string, returning nil if empty.
// This is a convenience function for converting optional string pin paths.
func NewLinkPath(s string) *LinkPath {
	if s == "" {
		return nil
	}
	p := LinkPath(s)
	return &p
}

// IsMounted reports whether a bpffs is mounted at mountPoint by
// parsing mountInfoPath (e.g. /proc/self/mountinfo).
//
// The mountinfo format is documented in proc(5). Each line contains:
//
//	mount_id parent_id major:minor root mount_point options [optional_fields...] - fstype source super_options
//
// Example bpffs entry:
//
//	30 22 0:27 / /sys/fs/bpf rw,nosuid shared:9 - bpf bpf rw,mode=700
//	              ↑                               ↑
//	              mount_point (fields[4])         fstype (after " - ")
//
// The key insight from libmount (util-linux) is that the separator "
// - " must be found using string search, not by assuming a fixed
// field position. This is because optional fields (like "shared:N"
// for mount propagation) may be present between the mount options and
// the separator.
func IsMounted(mountInfoPath, mountPoint string) (bool, error) {
	file, err := os.Open(mountInfoPath)
	if err != nil {
		return false, fmt.Errorf("opening mountinfo: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), defaultScanMaxLineLen)

	for scanner.Scan() {
		line := scanner.Text()

		// Find the separator " - " which precedes "fstype
		// source super_options". This is how libmount parses
		// mountinfo (see mnt_parse_mountinfo_line).
		sepIdx := strings.Index(line, " - ")
		if sepIdx == -1 {
			continue
		}

		// Parse the prefix: mount_id parent_id major:minor
		// root mount_point ...
		prefix := line[:sepIdx]
		fields := strings.Fields(prefix)
		if len(fields) < 5 {
			continue
		}
		mntPoint := unescapeMountInfo(fields[4])

		// Parse the suffix after " - ": fstype source
		// super_options.
		suffix := line[sepIdx+3:] // skip " - "
		suffixFields := strings.Fields(suffix)
		if len(suffixFields) < 1 {
			continue
		}
		fsType := suffixFields[0]

		// Match: bpffs at the requested path.
		if mntPoint == mountPoint && fsType == "bpf" {
			return true, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("reading mountinfo: %w", err)
	}

	return false, nil
}

// Mount mounts a bpffs at mountPoint, creating the directory if needed.
func Mount(mountPoint string) error {
	fi, err := os.Stat(mountPoint)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("mount point exists but is not a directory")
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(mountPoint, 0755); err != nil {
			return fmt.Errorf("creating mount point directory: %w", err)
		}
	default:
		return fmt.Errorf("stat mount point: %w", err)
	}

	if err := syscall.Mount("bpffs", mountPoint, "bpf", 0, ""); err != nil {
		return fmt.Errorf("mount syscall: %w", err)
	}

	return nil
}

// Unmount unmounts the bpffs at mountPoint.
func Unmount(mountPoint string) error {
	if err := syscall.Unmount(mountPoint, 0); err != nil {
		return fmt.Errorf("unmount syscall: %w", err)
	}
	return nil
}

// EnsureMounted ensures a bpffs is mounted at mountPoint. It checks
// mountInfoPath (e.g. /proc/self/mountinfo) for an existing bpf mount
// at mountPoint; if none is found, it mounts one.
//
// Equivalent to:
//
//	if ! findmnt --noheadings --types bpf <mountPoint>; then
//	  mount bpffs <mountPoint> -t bpf
//	fi
func EnsureMounted(mountInfoPath, mountPoint string) error {
	return EnsureMountedWith(mountInfoPath, mountPoint, Mount)
}

// EnsureMountedWith is like EnsureMounted but accepts a mount function.
// This is primarily for tests that need to simulate mount errors.
func EnsureMountedWith(mountInfoPath, mountPoint string, mountFn func(string) error) error {
	mounted, err := IsMounted(mountInfoPath, mountPoint)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := mountFn(mountPoint); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			mounted, recheckErr := IsMounted(mountInfoPath, mountPoint)
			if recheckErr == nil && mounted {
				return nil
			}
		}
		return err
	}
	return nil
}

// unescapeMountInfo converts an escaped mountinfo field into its literal form.
// The kernel escapes space, tab, newline, and backslash in mount point fields
// using 3-digit octal sequences (e.g., "\040" for space). See mangle_path() in
// fs/seq_file.c and its usage in fs/proc_namespace.c.
// This mirrors util-linux/libmount's handling of mountinfo escaping.
//
// We unescape because comparisons against mountPoint should use the literal
// path as provided by callers, not the escaped representation from /proc.
//
// Examples:
//   - "/sys/fs/bpf\\040extra" -> "/sys/fs/bpf extra"
//   - "tab\\011sep" -> "tab\tsep"
//   - "newline\\012here" -> "newline\nhere"
//   - "backslash\\134path" -> "backslash\\path"
//
// The logic scans for backslash-escaped octal triplets and replaces them with
// the corresponding byte. Non-escape sequences are left as-is. This matches
// util-linux's unmangle_to_buffer() in lib/mangle.c, which only decodes
// backslash plus three octal digits and leaves other sequences untouched.
func unescapeMountInfo(s string) string {
	if strings.IndexByte(s, '\\') == -1 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		if s[i+1] < '0' || s[i+1] > '7' || s[i+2] < '0' || s[i+2] > '7' || s[i+3] < '0' || s[i+3] > '7' {
			b.WriteByte(s[i])
			continue
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String()
}
