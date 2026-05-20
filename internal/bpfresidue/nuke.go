package bpfresidue

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/frobware/go-bpfman/fs"
)

// ScanNuke returns a Plan that removes bpfman's runtime
// directory contents wholesale: every file under the bpf fs
// mount point, the SQLite database (plus its WAL and shared
// memory files), and every bytecode cache directory.
//
// Unlike ScanE2EResidue and PlanFromObservation, ScanNuke does
// not consult the store. It is the escape hatch for when the
// store and the bpf fs have drifted out of sync to a state
// where the normal cleanup flows cannot reconcile them. Next
// `bpfman` invocation rebuilds a clean tree.
//
// The bpf fs mount point itself is not removed -- the mount is
// still there. Only its contents are cleared.
func ScanNuke(layout fs.Layout) (Plan, error) {
	var plan Plan

	bpffsRoot := layout.BPFFS().MountPoint()
	entries, err := os.ReadDir(bpffsRoot)
	if err != nil && !os.IsNotExist(err) {
		return plan, fmt.Errorf("read bpffs root %s: %w", bpffsRoot, err)
	}
	for _, e := range entries {
		plan = append(plan, RemoveTree{Path: filepath.Join(bpffsRoot, e.Name())})
	}

	dbPath := layout.DBPath()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := dbPath + suffix
		if _, err := os.Stat(path); err == nil {
			plan = append(plan, RemovePin{Path: path})
		}
	}

	bytecodeRoot := filepath.Join(layout.Base(), "programs")
	cacheEntries, err := os.ReadDir(bytecodeRoot)
	if err != nil && !os.IsNotExist(err) {
		return plan, fmt.Errorf("read %s: %w", bytecodeRoot, err)
	}
	for _, e := range cacheEntries {
		plan = append(plan, RemoveTree{Path: filepath.Join(bytecodeRoot, e.Name())})
	}

	return plan, nil
}
