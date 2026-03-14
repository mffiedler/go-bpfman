package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
)

// ProgramDetail carries everything needed for all show sub-views.
type ProgramDetail struct {
	Program  bpfman.Program `json:"program"`
	ProgPin  PathEntry      `json:"prog_pin"`
	MapDir   PathEntry      `json:"map_dir"`
	LinkDir  PathEntry      `json:"link_dir"`
	Bytecode PathEntry      `json:"bytecode"`
	Maps     []MapDetail    `json:"maps,omitempty"`
	Links    []LinkDetail   `json:"links,omitempty"`
}

// PathEntry pairs a filesystem path with its presence status.
type PathEntry struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

// MapDetail enriches a kernel map with its pin path and presence.
type MapDetail struct {
	kernel.Map
	PinPath string `json:"pin_path"`
	Present bool   `json:"present"`
}

// LinkDetail enriches a bpfman link with its pin path and presence.
type LinkDetail struct {
	bpfman.Link
	PinPath string `json:"pin_path"`
	Present bool   `json:"present"`
}

// buildProgramDetail fetches a program via mgr.Get and enriches it
// with BPFFS pin paths and presence checks for all sub-views.
func buildProgramDetail(ctx context.Context, mgr *manager.Manager, programID kernel.ProgramID) (ProgramDetail, error) {
	prog, err := mgr.Get(ctx, programID)
	if err != nil {
		return ProgramDetail{}, fmt.Errorf("get program %d: %w", programID, err)
	}

	bpffs := mgr.Runtime().BPFFS()
	scanner := bpffs.Scanner()
	bc := mgr.Runtime().Bytecode()

	// Determine map owner for map path construction.
	mapOwner := programID
	if prog.Record.Handles.MapOwnerID != nil {
		mapOwner = *prog.Record.Handles.MapOwnerID
	}

	detail := ProgramDetail{
		Program: prog,
		ProgPin: PathEntry{
			Path:    bpffs.ProgPinPath(programID),
			Present: scanner.PathExists(bpffs.ProgPinPath(programID)),
		},
		MapDir: PathEntry{
			Path:    bpffs.MapPinDir(mapOwner),
			Present: scanner.PathExists(bpffs.MapPinDir(mapOwner)),
		},
		LinkDir: PathEntry{
			Path:    bpffs.LinkPinDir(programID),
			Present: scanner.PathExists(bpffs.LinkPinDir(programID)),
		},
		Bytecode: PathEntry{
			Path:    bc.ProgramDir(programID),
			Present: bc.ProgramExists(programID),
		},
	}

	// Derive map pins from the filesystem directory rather than
	// constructing paths from kernel-truncated names. The kernel
	// truncates map names to 15 characters, but pins use the full
	// ELF section name.
	mapDir := bpffs.MapPinDir(mapOwner)
	if entries, err := os.ReadDir(mapDir); err == nil {
		matched := make(map[int]bool)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			pinPath := filepath.Join(mapDir, name)

			md := MapDetail{
				PinPath: pinPath,
				Present: true,
			}

			// Correlate with kernel map: the kernel truncates
			// names to 15 chars, so the kernel name is a prefix
			// of the full ELF name.
			for i, km := range prog.Status.Maps {
				if matched[i] {
					continue
				}
				if name == km.Name || strings.HasPrefix(name, km.Name) {
					md.Map = km
					matched[i] = true
					break
				}
			}

			detail.Maps = append(detail.Maps, md)
		}

		// Report kernel maps with no corresponding pin.
		for i, km := range prog.Status.Maps {
			if !matched[i] {
				pinPath := bpffs.MapPinPath(mapOwner, km.Name)
				detail.Maps = append(detail.Maps, MapDetail{
					Map:     km,
					PinPath: pinPath,
					Present: false,
				})
			}
		}
	} else {
		// Directory unreadable or absent: fall back to
		// constructing paths from kernel names.
		for _, m := range prog.Status.Maps {
			pinPath := bpffs.MapPinPath(mapOwner, m.Name)
			detail.Maps = append(detail.Maps, MapDetail{
				Map:     m,
				PinPath: pinPath,
				Present: scanner.PathExists(pinPath),
			})
		}
	}

	// Enrich each link with its pin path.
	for _, l := range prog.Status.Links {
		var pinPath string
		var present bool
		if l.Record.PinPath != nil {
			pinPath = l.Record.PinPath.String()
			present = scanner.PathExists(pinPath)
		}
		detail.Links = append(detail.Links, LinkDetail{
			Link:    l,
			PinPath: pinPath,
			Present: present,
		})
	}

	return detail, nil
}
