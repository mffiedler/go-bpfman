package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sys/unix"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/kernel"
)

// ErrMultipleProgramsFound is returned when multiple programs match the
// search criteria and none is the map owner.
var ErrMultipleProgramsFound = errors.New("multiple programs found")

// ErrMultipleMapOwners is returned when multiple programs claim to be
// the map owner (MapOwnerID == 0). This indicates a data inconsistency.
var ErrMultipleMapOwners = errors.New("multiple map owners found")

// GetHostInfo returns system information from uname.
func GetHostInfo() bpfman.HostInfo {
	var utsname unix.Utsname
	if err := unix.Uname(&utsname); err != nil {
		return bpfman.HostInfo{}
	}
	return bpfman.HostInfo{
		Sysname:  unix.ByteSliceToString(utsname.Sysname[:]),
		Nodename: unix.ByteSliceToString(utsname.Nodename[:]),
		Release:  unix.ByteSliceToString(utsname.Release[:]),
		Version:  unix.ByteSliceToString(utsname.Version[:]),
		Machine:  unix.ByteSliceToString(utsname.Machine[:]),
	}
}

// Get retrieves a managed program by its kernel ID.
// Returns the canonical bpfman.Program type with both Spec (from store)
// and Status (from kernel enumeration + filesystem checks + links + maps).
// Returns an error if the program exists in the store but not in the kernel,
// as this indicates an inconsistent state that requires reconciliation.
func (m *Manager) Get(ctx context.Context, kernelID uint32) (bpfman.Program, error) {
	// Fetch program from store
	metadata, err := m.store.Get(ctx, kernelID)
	if err != nil {
		return bpfman.Program{}, err
	}

	// Fetch program from kernel
	kp, err := m.kernel.GetProgramByID(ctx, kernelID)
	if err != nil {
		return bpfman.Program{}, fmt.Errorf("program %d exists in store but not in kernel (requires reconciliation): %w", kernelID, err)
	}

	// Fetch links from store (records with details)
	storedLinks, err := m.store.ListLinksByProgram(ctx, kernelID)
	if err != nil {
		return bpfman.Program{}, fmt.Errorf("list links: %w", err)
	}

	// Build links with spec + status
	var links []bpfman.Link
	for _, sl := range storedLinks {
		// Fetch full record with details for this link
		record, err := m.store.GetLink(ctx, sl.ID)
		if err != nil {
			m.logger.WarnContext(ctx, "failed to get link details", "link_id", sl.ID, "error", err)
			record = sl // Use summary record without details
		}

		link := bpfman.Link{
			Record: record,
			Status: bpfman.LinkStatus{
				PinPresent: record.PinPath != nil,
			},
		}

		// Fetch kernel link if non-synthetic
		if !record.IsSynthetic() {
			kl, err := m.kernel.GetLinkByID(ctx, uint32(record.ID))
			if err == nil {
				link.Status.Kernel = &kl
			}
		}

		links = append(links, link)
	}

	// Fetch each map from kernel using the program's map IDs
	var kernelMaps []kernel.Map
	for _, mapID := range kp.MapIDs {
		km, err := m.kernel.GetMapByID(ctx, mapID)
		if err != nil {
			// Map exists in program but not accessible - skip
			continue
		}
		kernelMaps = append(kernelMaps, km)
	}

	// Fetch stats (best-effort, don't fail if unavailable)
	var stats *kernel.ProgramStats
	if s, err := m.kernel.GetProgramStatsByID(ctx, kernelID); err == nil {
		stats = s
	}

	return bpfman.Program{
		Record: metadata,
		Status: bpfman.ProgramStatus{
			Kernel:      &kp,
			Stats:       stats,
			PinPresent:  true, // If we got here, program exists
			MapsPresent: len(kernelMaps) > 0,
			Links:       links,
			Maps:        kernelMaps,
		},
	}, nil
}

// ListLinks returns all managed links (records only).
// Optional LinkListOption arguments filter the results.
func (m *Manager) ListLinks(ctx context.Context, opts ...bpfman.LinkListOption) ([]bpfman.LinkRecord, error) {
	links, err := m.store.ListLinks(ctx)
	if err != nil {
		return nil, err
	}

	filter := bpfman.ApplyLinkListOptions(opts...)

	var result []bpfman.LinkRecord
	for _, link := range links {
		l := link // explicit copy
		if filter.Matches(&l) {
			result = append(result, link)
		}
	}
	return result, nil
}

// ListLinksByProgram returns all links for a given program.
func (m *Manager) ListLinksByProgram(ctx context.Context, programKernelID uint32) ([]bpfman.LinkRecord, error) {
	return m.store.ListLinksByProgram(ctx, programKernelID)
}

// GetLink retrieves a link by link ID, returning the full record with details.
func (m *Manager) GetLink(ctx context.Context, linkID bpfman.LinkID) (bpfman.LinkRecord, error) {
	record, err := m.store.GetLink(ctx, linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bpfman.LinkRecord{}, bpfman.ErrLinkNotFound{LinkID: linkID}
		}
		return bpfman.LinkRecord{}, fmt.Errorf("get link %d: %w", linkID, err)
	}
	return record, nil
}

// GetLinkInfo retrieves a link with presence information across store, kernel, and filesystem.
func (m *Manager) GetLinkInfo(ctx context.Context, linkID bpfman.LinkID) (inspect.LinkInfo, error) {
	scanner := m.fsctx.BPFFS().Scanner()
	return inspect.GetLink(ctx, m.store, m.kernel, scanner, linkID)
}

// FindLoadedProgramByMetadata finds a program by metadata key/value from
// the reconciled list of loaded programs (those in both DB and kernel).
//
// When multiple programs match (e.g., multi-program applications), this
// returns the map owner (the program with MapOwnerID == 0). All maps are
// pinned at the owner's MapPinPath, so the CSI can find them there.
//
// Returns an error if no programs match, or if multiple map owners exist
// (data inconsistency).
func (m *Manager) FindLoadedProgramByMetadata(ctx context.Context, key, value string) (bpfman.ProgramRecord, uint32, error) {
	scanner := m.fsctx.BPFFS().Scanner()
	world, err := inspect.Snapshot(ctx, m.store, m.kernel, scanner)
	if err != nil {
		return bpfman.ProgramRecord{}, 0, fmt.Errorf("snapshot: %w", err)
	}

	// Find managed programs that are also in kernel and match the metadata
	var matches []inspect.ProgramView
	for _, row := range world.Programs {
		if !row.Presence.InStore || !row.Presence.InKernel {
			continue
		}
		if row.Managed.Meta.Metadata[key] == value {
			matches = append(matches, row)
		}
	}

	switch len(matches) {
	case 0:
		return bpfman.ProgramRecord{}, 0, fmt.Errorf("program with %s=%s: %w", key, value, store.ErrNotFound)
	case 1:
		return *matches[0].Managed, matches[0].KernelID, nil
	default:
		// Multiple programs match - find the map owner (MapOwnerID == nil).
		// In multi-program loads, one program owns all maps and the others
		// reference it via MapOwnerID.
		var owners []inspect.ProgramView
		for _, row := range matches {
			if row.Managed.Handles.MapOwnerID == nil {
				owners = append(owners, row)
			}
		}

		switch len(owners) {
		case 0:
			// No map owner found - all programs reference another owner
			// that doesn't match our metadata query. This shouldn't happen.
			ids := make([]uint32, len(matches))
			for i, row := range matches {
				ids[i] = row.KernelID
			}
			return bpfman.ProgramRecord{}, 0, fmt.Errorf("%w: %d programs with %s=%s but no map owner (kernel IDs: %v)",
				ErrMultipleProgramsFound, len(matches), key, value, ids)
		case 1:
			m.logger.DebugContext(ctx, "found map owner among multiple matching programs",
				"key", key,
				"value", value,
				"total_matches", len(matches),
				"owner_kernel_id", owners[0].KernelID,
				"owner_name", owners[0].Managed.Meta.Name,
			)
			return *owners[0].Managed, owners[0].KernelID, nil
		default:
			// Multiple map owners - data inconsistency
			ids := make([]uint32, len(owners))
			for i, row := range owners {
				ids[i] = row.KernelID
			}
			return bpfman.ProgramRecord{}, 0, fmt.Errorf("%w: %d map owners with %s=%s (kernel IDs: %v)",
				ErrMultipleMapOwners, len(owners), key, value, ids)
		}
	}
}

// ListPrograms returns all managed programs with full spec and status.
// This returns the canonical bpfman.ProgramListResult type with both Spec (from store)
// and Status (from kernel enumeration + filesystem checks).
// Optional ListOption arguments filter the results.
// Results are ordered deterministically by kernel ID, then by type+name for ties.
func (m *Manager) ListPrograms(ctx context.Context, opts ...bpfman.ListOption) (bpfman.ProgramListResult, error) {
	filter := bpfman.ApplyListOptions(opts...)

	scanner := m.fsctx.BPFFS().Scanner()
	world, err := inspect.Snapshot(ctx, m.store, m.kernel, scanner)
	if err != nil {
		return bpfman.ProgramListResult{}, fmt.Errorf("snapshot: %w", err)
	}

	var programs []bpfman.Program
	for _, row := range world.ManagedPrograms() {
		if prog, ok := row.AsProgram(); ok {
			p := prog // explicit copy for clarity
			if filter.Matches(&p) {
				programs = append(programs, p)
			}
		}
	}

	// Deterministic output ordering: by kernel ID, then by type+name for ties
	sort.Slice(programs, func(i, j int) bool {
		if programs[i].Record.KernelID != programs[j].Record.KernelID {
			return programs[i].Record.KernelID < programs[j].Record.KernelID
		}
		// Fallback for zero IDs: sort by type, then name
		if programs[i].Record.Load.ProgramType() != programs[j].Record.Load.ProgramType() {
			return programs[i].Record.Load.ProgramType() < programs[j].Record.Load.ProgramType()
		}
		return programs[i].Record.Meta.Name < programs[j].Record.Meta.Name
	})

	return bpfman.ProgramListResult{
		ObservedAt: world.Meta.ObservedAt,
		Host:       GetHostInfo(),
		Programs:   programs,
	}, nil
}
