package inspect

import (
	"context"
	"errors"
	iofs "io/fs"
	"path/filepath"

	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
)

// linkPinIndex maps a kernel link ID to the bpf fs pin path
// backing it. Populated by walking the bpfman bpf fs subtree
// in scanLinkPins and used to set FSPinPath on every LinkRow
// the snapshot returns.
//
// Snapshot needs this because the per-program LinkDirs scan in
// scanner.go only enters {fs}/links/<program_id>/; extension
// link pins under {fs}/{type}/dispatcher_*/link_N and TCX link
// pins under {fs}/tcx-{direction}/ never appear in that scan,
// so a kernel-only link backed by one of those pins would
// otherwise look unpinned in the snapshot.
type linkPinIndex map[kernel.LinkID]string

// scanLinkPins walks scanner.MountPoint() recursively and
// attempts link.LoadPinnedLink on every file it finds. Each
// successful load contributes one entry to the index; non-link
// pins (programs, maps) and stale entries fail to load and are
// skipped silently. Returns an empty index when the bpf fs
// mount point does not exist.
//
// One pin per link ID is recorded. bpfman pins each link in at
// most one location, so duplicate keys do not happen in
// practice; if they ever do, the first walk hit wins, which
// is deterministic enough for diff purposes.
func scanLinkPins(ctx context.Context, scanner *fs.Scanner) (linkPinIndex, error) {
	idx := make(linkPinIndex)
	root := scanner.MountPoint()
	err := filepath.WalkDir(root, func(path string, d iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, iofs.ErrNotExist) {
				return nil
			}
			// Skip an unreadable entry but keep walking the rest of
			// the tree -- the snapshot is best-effort.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lnk, lerr := link.LoadPinnedLink(path, nil)
		if lerr != nil {
			return nil
		}
		info, ierr := lnk.Info()
		lnk.Close()
		if ierr != nil {
			return nil
		}
		id := kernel.LinkID(info.ID)
		if _, seen := idx[id]; !seen {
			idx[id] = path
		}
		return nil
	})
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return idx, err
	}
	return idx, nil
}
