package bpfmanfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RemovePinFile removes a pin file under the bpffs mount.
//
// Safety properties:
//   - Refuses to operate outside the bpffs mount.
//   - Refuses to remove the mount root.
//   - Ignores ENOENT for idempotent GC.
func (b BPFFS) RemovePinFile(path string) error {
	path, err := b.cleanUnderMount(path)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pin %s: %w", path, err)
	}
	return nil
}

// RemoveDir removes a directory tree under the bpffs mount.
//
// This is the only place we allow RemoveAll. Callers should not call
// os.RemoveAll directly.
//
// Safety properties:
//   - Refuses to operate outside the bpffs mount.
//   - Refuses to remove the mount root.
//   - Ignores ENOENT for idempotent GC.
func (b BPFFS) RemoveDir(path string) error {
	path, err := b.cleanUnderMount(path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove dir %s: %w", path, err)
	}
	return nil
}

// RemoveProgPin removes a bpfman program pin of the form:
//
//	{bpffs}/prog_{kernel_id}
func (b BPFFS) RemoveProgPin(path string) error {
	path, err := b.cleanUnderMount(path)
	if err != nil {
		return err
	}
	if filepath.Dir(path) != b.MountPoint() {
		return fmt.Errorf("prog pin not under mount root: %s", path)
	}
	if !strings.HasPrefix(filepath.Base(path), "prog_") {
		return fmt.Errorf("prog pin has unexpected name: %s", path)
	}
	return b.RemovePinFile(path)
}

// RemoveLinkDir removes a link directory of the form:
//
//	{bpffs}/links/{program_id}
func (b BPFFS) RemoveLinkDir(path string) error {
	return b.removeNumericChildDir(b.Links(), path, "link dir")
}

// RemoveMapDir removes a map directory of the form:
//
//	{bpffs}/maps/{program_id}
func (b BPFFS) RemoveMapDir(path string) error {
	return b.removeNumericChildDir(b.Maps(), path, "map dir")
}

// RemoveDispatcherRevDir removes a dispatcher revision directory of
// the form:
//
//	{bpffs}/{type}/dispatcher_{nsid}_{ifindex}_{revision}
//
// The directory is owned by bpfman and safe to delete when deemed
// stale or orphaned.
func (b BPFFS) RemoveDispatcherRevDir(path string) error {
	path, err := b.cleanUnderMount(path)
	if err != nil {
		return err
	}

	parent := filepath.Dir(path)
	if !b.isDispatcherTypeDir(parent) {
		return fmt.Errorf("dispatcher dir not under type dir: %s", path)
	}

	base := filepath.Base(path)
	if !strings.HasPrefix(base, "dispatcher_") {
		return fmt.Errorf("dispatcher dir has unexpected name: %s", path)
	}

	// Validate suffixes are numeric to avoid rm -rf surprises.
	// Expected: dispatcher_NSID_IFINDEX_REV.
	parts := strings.Split(strings.TrimPrefix(base, "dispatcher_"), "_")
	if len(parts) != 3 {
		return fmt.Errorf("dispatcher dir has unexpected name: %s", path)
	}
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 10, 64); err != nil {
			return fmt.Errorf("dispatcher dir has unexpected name: %s", path)
		}
	}

	return b.RemoveDir(path)
}

// RemoveDispatcherLinkPin removes a dispatcher link pin of the form:
//
//	{bpffs}/{type}/dispatcher_{nsid}_{ifindex}_link
func (b BPFFS) RemoveDispatcherLinkPin(path string) error {
	path, err := b.cleanUnderMount(path)
	if err != nil {
		return err
	}

	parent := filepath.Dir(path)
	if !b.isDispatcherTypeDir(parent) {
		return fmt.Errorf("dispatcher link not under type dir: %s", path)
	}

	base := filepath.Base(path)
	if !strings.HasPrefix(base, "dispatcher_") || !strings.HasSuffix(base, "_link") {
		return fmt.Errorf("dispatcher link has unexpected name: %s", path)
	}

	trim := strings.TrimSuffix(strings.TrimPrefix(base, "dispatcher_"), "_link")
	parts := strings.Split(trim, "_")
	if len(parts) != 2 {
		return fmt.Errorf("dispatcher link has unexpected name: %s", path)
	}
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 10, 64); err != nil {
			return fmt.Errorf("dispatcher link has unexpected name: %s", path)
		}
	}

	return b.RemovePinFile(path)
}

// removeNumericChildDir removes a directory that is a direct child of
// parent and has a numeric name.
func (b BPFFS) removeNumericChildDir(parent, path, what string) error {
	path, err := b.cleanUnderMount(path)
	if err != nil {
		return err
	}
	if filepath.Dir(path) != parent {
		return fmt.Errorf("%s not directly under %s: %s", what, parent, path)
	}
	if _, err := strconv.ParseUint(filepath.Base(path), 10, 32); err != nil {
		return fmt.Errorf("%s has non-numeric name: %s", what, path)
	}
	return b.RemoveDir(path)
}

// isDispatcherTypeDir returns true if path is one of the dispatcher
// type directories (xdp, tc-ingress, tc-egress).
func (b BPFFS) isDispatcherTypeDir(path string) bool {
	switch path {
	case b.XDP(), b.TCIngress(), b.TCEgress():
		return true
	default:
		return false
	}
}

// cleanUnderMount validates and cleans a path, ensuring it is under
// the bpffs mount point.
func (b BPFFS) cleanUnderMount(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}

	mount := filepath.Clean(b.MountPoint())
	clean := filepath.Clean(path)

	// Refuse to delete the mount root itself.
	if clean == mount {
		return "", fmt.Errorf("refusing to remove bpffs mount root: %s", clean)
	}

	// Ensure the path is within the mount.
	prefix := mount + string(os.PathSeparator)
	if !strings.HasPrefix(clean, prefix) {
		return "", fmt.Errorf("path escapes bpffs mount: %s", clean)
	}

	return clean, nil
}
