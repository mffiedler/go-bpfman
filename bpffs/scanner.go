package bpffs

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ScannerDirs holds the directory paths needed by Scanner.
// This avoids importing the config package, preventing import cycles.
type ScannerDirs struct {
	// FS is the bpffs mount point (e.g., /run/bpfman/fs).
	FS string `json:"fs"`
	// XDP is the XDP dispatcher directory.
	XDP string `json:"xdp"`
	// TCIngress is the TC ingress dispatcher directory.
	TCIngress string `json:"tc_ingress"`
	// TCEgress is the TC egress dispatcher directory.
	TCEgress string `json:"tc_egress"`
	// Maps is the maps directory.
	Maps string `json:"maps"`
	// Links is the links directory.
	Links string `json:"links"`
}

// Scanner provides read-only access to bpfman's filesystem layout.
// It encapsulates path conventions and provides streaming iterators
// for filesystem facts.
type Scanner struct {
	dirs        ScannerDirs
	onMalformed func(path string, err error)
}

// NewScanner creates a Scanner for the given directories.
func NewScanner(dirs ScannerDirs) *Scanner {
	return &Scanner{dirs: dirs}
}

// WithOnMalformed sets a callback for unparseable filesystem entries.
// The callback receives the path and the parse error. Returns the
// Scanner for chaining.
func (s *Scanner) WithOnMalformed(f func(path string, err error)) *Scanner {
	s.onMalformed = f
	return s
}

// reportMalformed calls the OnMalformed callback if set.
func (s *Scanner) reportMalformed(path string, err error) {
	if s.onMalformed != nil {
		s.onMalformed(path, err)
	}
}

// ProgPin represents a program pin: {dirs.FS}/prog_{kernel_id}
type ProgPin struct {
	Path     string `json:"path"`
	KernelID uint32 `json:"kernel_id"`
}

// LinkDir represents a link directory: {dirs.FS}/links/{program_id}
type LinkDir struct {
	Path      string `json:"path"`
	ProgramID uint32 `json:"program_id"`
}

// MapDir represents a map directory: {dirs.FS}/maps/{program_id}
type MapDir struct {
	Path      string `json:"path"`
	ProgramID uint32 `json:"program_id"`
}

// DispatcherDir represents a dispatcher revision directory.
// Path: {dirs.FS}/{type}/dispatcher_{nsid}_{ifindex}_{revision}
// LinkCount is derived by counting link_* files in the directory.
type DispatcherDir struct {
	Path      string `json:"path"`
	DispType  string `json:"disp_type"` // "xdp", "tc-ingress", "tc-egress"
	Nsid      uint64 `json:"nsid"`
	Ifindex   uint32 `json:"ifindex"`
	Revision  uint32 `json:"revision"`
	LinkCount int    `json:"link_count"`
}

// DispatcherLinkPin represents a dispatcher link pin (XDP only).
// Path: {dirs.FS}/{type}/dispatcher_{nsid}_{ifindex}_link
type DispatcherLinkPin struct {
	Path     string `json:"path"`
	DispType string `json:"disp_type"`
	Nsid     uint64 `json:"nsid"`
	Ifindex  uint32 `json:"ifindex"`
}

// FSState is a materialised snapshot of the filesystem.
// Use Scanner.Scan() to create, or construct directly in tests.
type FSState struct {
	ProgPins           []ProgPin           `json:"prog_pins"`
	LinkDirs           []LinkDir           `json:"link_dirs"`
	MapDirs            []MapDir            `json:"map_dirs"`
	DispatcherDirs     []DispatcherDir     `json:"dispatcher_dirs"`
	DispatcherLinkPins []DispatcherLinkPin `json:"dispatcher_link_pins"`
}

// ProgPins returns an iterator over program pins in {dirs.FS}/prog_*.
// Errors are yielded only for failures that prevent enumeration.
// Malformed entries are skipped and reported via OnMalformed.
func (s *Scanner) ProgPins(ctx context.Context) iter.Seq2[ProgPin, error] {
	return func(yield func(ProgPin, error) bool) {
		entries, err := os.ReadDir(s.dirs.FS)
		if err != nil {
			if os.IsNotExist(err) {
				return // directory doesn't exist: no pins
			}
			yield(ProgPin{}, fmt.Errorf("read dir %s: %w", s.dirs.FS, err))
			return
		}

		for _, entry := range entries {
			if ctx.Err() != nil {
				yield(ProgPin{}, ctx.Err())
				return
			}

			name := entry.Name()
			if !strings.HasPrefix(name, "prog_") {
				continue
			}

			suffix := strings.TrimPrefix(name, "prog_")
			id, err := strconv.ParseUint(suffix, 10, 32)
			if err != nil {
				s.reportMalformed(filepath.Join(s.dirs.FS, name), fmt.Errorf("parse kernel ID: %w", err))
				continue
			}

			pin := ProgPin{
				Path:     filepath.Join(s.dirs.FS, name),
				KernelID: uint32(id),
			}
			if !yield(pin, nil) {
				return
			}
		}
	}
}

// LinkDirs returns an iterator over link directories in {dirs.FS}/links/{program_id}.
// Errors are yielded only for failures that prevent enumeration.
// Malformed entries are skipped and reported via OnMalformed.
func (s *Scanner) LinkDirs(ctx context.Context) iter.Seq2[LinkDir, error] {
	return func(yield func(LinkDir, error) bool) {
		entries, err := os.ReadDir(s.dirs.Links)
		if err != nil {
			if os.IsNotExist(err) {
				return // directory doesn't exist: no link dirs
			}
			yield(LinkDir{}, fmt.Errorf("read dir %s: %w", s.dirs.Links, err))
			return
		}

		for _, entry := range entries {
			if ctx.Err() != nil {
				yield(LinkDir{}, ctx.Err())
				return
			}

			if !entry.IsDir() {
				continue
			}

			name := entry.Name()
			id, err := strconv.ParseUint(name, 10, 32)
			if err != nil {
				s.reportMalformed(filepath.Join(s.dirs.Links, name), fmt.Errorf("parse program ID: %w", err))
				continue
			}

			dir := LinkDir{
				Path:      filepath.Join(s.dirs.Links, name),
				ProgramID: uint32(id),
			}
			if !yield(dir, nil) {
				return
			}
		}
	}
}

// MapDirs returns an iterator over map directories in {dirs.FS}/maps/{program_id}.
// Errors are yielded only for failures that prevent enumeration.
// Malformed entries are skipped and reported via OnMalformed.
func (s *Scanner) MapDirs(ctx context.Context) iter.Seq2[MapDir, error] {
	return func(yield func(MapDir, error) bool) {
		entries, err := os.ReadDir(s.dirs.Maps)
		if err != nil {
			if os.IsNotExist(err) {
				return // directory doesn't exist: no map dirs
			}
			yield(MapDir{}, fmt.Errorf("read dir %s: %w", s.dirs.Maps, err))
			return
		}

		for _, entry := range entries {
			if ctx.Err() != nil {
				yield(MapDir{}, ctx.Err())
				return
			}

			if !entry.IsDir() {
				continue
			}

			name := entry.Name()
			id, err := strconv.ParseUint(name, 10, 32)
			if err != nil {
				s.reportMalformed(filepath.Join(s.dirs.Maps, name), fmt.Errorf("parse program ID: %w", err))
				continue
			}

			dir := MapDir{
				Path:      filepath.Join(s.dirs.Maps, name),
				ProgramID: uint32(id),
			}
			if !yield(dir, nil) {
				return
			}
		}
	}
}

// DispatcherDirs returns an iterator over dispatcher revision directories.
// Path pattern: {dirs.FS}/{type}/dispatcher_{nsid}_{ifindex}_{revision}
// LinkCount is the number of link_* files in each directory.
// Errors are yielded only for failures that prevent enumeration.
// Malformed entries are skipped and reported via OnMalformed.
func (s *Scanner) DispatcherDirs(ctx context.Context) iter.Seq2[DispatcherDir, error] {
	return func(yield func(DispatcherDir, error) bool) {
		dispTypes := []struct {
			name string
			dir  string
		}{
			{"xdp", s.dirs.XDP},
			{"tc-ingress", s.dirs.TCIngress},
			{"tc-egress", s.dirs.TCEgress},
		}

		for _, t := range dispTypes {
			if ctx.Err() != nil {
				yield(DispatcherDir{}, ctx.Err())
				return
			}

			entries, err := os.ReadDir(t.dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue // directory doesn't exist
				}
				if !yield(DispatcherDir{}, fmt.Errorf("read dir %s: %w", t.dir, err)) {
					return
				}
				continue
			}

			for _, entry := range entries {
				if ctx.Err() != nil {
					yield(DispatcherDir{}, ctx.Err())
					return
				}

				if !entry.IsDir() {
					continue
				}

				name := entry.Name()
				if !strings.HasPrefix(name, "dispatcher_") {
					continue
				}

				// Parse dispatcher_{nsid}_{ifindex}_{revision}
				var nsid uint64
				var ifindex, revision uint32
				n, err := fmt.Sscanf(name, "dispatcher_%d_%d_%d", &nsid, &ifindex, &revision)
				if err != nil || n != 3 {
					s.reportMalformed(filepath.Join(t.dir, name), fmt.Errorf("parse dispatcher dir: expected dispatcher_NSID_IFINDEX_REV"))
					continue
				}

				dirPath := filepath.Join(t.dir, name)
				linkCount := s.countLinkFiles(dirPath)

				dir := DispatcherDir{
					Path:      dirPath,
					DispType:  t.name,
					Nsid:      nsid,
					Ifindex:   ifindex,
					Revision:  revision,
					LinkCount: linkCount,
				}
				if !yield(dir, nil) {
					return
				}
			}
		}
	}
}

// countLinkFiles counts link_* files in a directory.
func (s *Scanner) countLinkFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "link_") {
			count++
		}
	}
	return count
}

// DispatcherLinkPins returns an iterator over dispatcher link pins.
// Path pattern: {dirs.FS}/{type}/dispatcher_{nsid}_{ifindex}_link
// Errors are yielded only for failures that prevent enumeration.
// Malformed entries are skipped and reported via OnMalformed.
func (s *Scanner) DispatcherLinkPins(ctx context.Context) iter.Seq2[DispatcherLinkPin, error] {
	return func(yield func(DispatcherLinkPin, error) bool) {
		dispTypes := []struct {
			name string
			dir  string
		}{
			{"xdp", s.dirs.XDP},
			{"tc-ingress", s.dirs.TCIngress},
			{"tc-egress", s.dirs.TCEgress},
		}

		for _, t := range dispTypes {
			if ctx.Err() != nil {
				yield(DispatcherLinkPin{}, ctx.Err())
				return
			}

			entries, err := os.ReadDir(t.dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				if !yield(DispatcherLinkPin{}, fmt.Errorf("read dir %s: %w", t.dir, err)) {
					return
				}
				continue
			}

			for _, entry := range entries {
				if ctx.Err() != nil {
					yield(DispatcherLinkPin{}, ctx.Err())
					return
				}

				if entry.IsDir() {
					continue
				}

				name := entry.Name()
				if !strings.HasPrefix(name, "dispatcher_") || !strings.HasSuffix(name, "_link") {
					continue
				}

				// Parse dispatcher_{nsid}_{ifindex}_link
				var nsid uint64
				var ifindex uint32
				n, err := fmt.Sscanf(name, "dispatcher_%d_%d_link", &nsid, &ifindex)
				if err != nil || n != 2 {
					s.reportMalformed(filepath.Join(t.dir, name), fmt.Errorf("parse dispatcher link pin: expected dispatcher_NSID_IFINDEX_link"))
					continue
				}

				pin := DispatcherLinkPin{
					Path:     filepath.Join(t.dir, name),
					DispType: t.name,
					Nsid:     nsid,
					Ifindex:  ifindex,
				}
				if !yield(pin, nil) {
					return
				}
			}
		}
	}
}

// PathExists checks if a path exists on the filesystem.
// Used to verify store-recorded pin paths actually exist.
func (s *Scanner) PathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Scan materialises everything into an FSState.
// Returns an error if any iterator encounters a fatal error.
func (s *Scanner) Scan(ctx context.Context) (*FSState, error) {
	state := &FSState{}

	for pin, err := range s.ProgPins(ctx) {
		if err != nil {
			return nil, fmt.Errorf("scan prog pins: %w", err)
		}
		state.ProgPins = append(state.ProgPins, pin)
	}

	for dir, err := range s.LinkDirs(ctx) {
		if err != nil {
			return nil, fmt.Errorf("scan link dirs: %w", err)
		}
		state.LinkDirs = append(state.LinkDirs, dir)
	}

	for dir, err := range s.MapDirs(ctx) {
		if err != nil {
			return nil, fmt.Errorf("scan map dirs: %w", err)
		}
		state.MapDirs = append(state.MapDirs, dir)
	}

	for dir, err := range s.DispatcherDirs(ctx) {
		if err != nil {
			return nil, fmt.Errorf("scan dispatcher dirs: %w", err)
		}
		state.DispatcherDirs = append(state.DispatcherDirs, dir)
	}

	for pin, err := range s.DispatcherLinkPins(ctx) {
		if err != nil {
			return nil, fmt.Errorf("scan dispatcher link pins: %w", err)
		}
		state.DispatcherLinkPins = append(state.DispatcherLinkPins, pin)
	}

	return state, nil
}
