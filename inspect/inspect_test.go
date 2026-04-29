package inspect_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// fakeStore implements StoreLister for testing.
type fakeStore struct {
	programs    map[kernel.ProgramID]bpfman.ProgramRecord
	links       []bpfman.LinkRecord
	dispatchers []platform.DispatcherSummary
}

func (s *fakeStore) List(ctx context.Context) (map[kernel.ProgramID]bpfman.ProgramRecord, error) {
	return s.programs, nil
}

func (s *fakeStore) Get(ctx context.Context, programID kernel.ProgramID) (bpfman.ProgramRecord, error) {
	if p, ok := s.programs[programID]; ok {
		return p, nil
	}
	return bpfman.ProgramRecord{}, platform.ErrRecordNotFound
}

func (s *fakeStore) ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error) {
	return s.links, nil
}

func (s *fakeStore) GetLink(ctx context.Context, linkID kernel.LinkID) (bpfman.LinkRecord, error) {
	for _, l := range s.links {
		if l.ID == linkID {
			return l, nil
		}
	}
	return bpfman.LinkRecord{}, platform.ErrRecordNotFound
}

func (s *fakeStore) ListDispatcherSummaries(ctx context.Context) ([]platform.DispatcherSummary, error) {
	return s.dispatchers, nil
}

// fakeKernelSource implements KernelLister for testing.
type fakeKernelSource struct {
	programs []kernel.Program
	links    []kernel.Link
}

func (k *fakeKernelSource) Programs(ctx context.Context) iter.Seq2[kernel.Program, error] {
	return func(yield func(kernel.Program, error) bool) {
		for _, p := range k.programs {
			if !yield(p, nil) {
				return
			}
		}
	}
}

func (k *fakeKernelSource) GetProgramByID(ctx context.Context, id kernel.ProgramID) (kernel.Program, error) {
	for _, p := range k.programs {
		if p.ID == id {
			return p, nil
		}
	}
	return kernel.Program{}, errors.New("program not found")
}

func (k *fakeKernelSource) Links(ctx context.Context) iter.Seq2[kernel.Link, error] {
	return func(yield func(kernel.Link, error) bool) {
		for _, l := range k.links {
			if !yield(l, nil) {
				return
			}
		}
	}
}

func (k *fakeKernelSource) GetLinkByID(ctx context.Context, id kernel.LinkID) (kernel.Link, error) {
	for _, l := range k.links {
		if l.ID == id {
			return l, nil
		}
	}
	return kernel.Link{}, errors.New("link not found")
}

// testBPFFS creates a BPFFS for testing with a temporary directory.
// Returns the BPFFS and a struct with convenient path accessors.
func testBPFFS(t *testing.T) fs.BPFFS {
	t.Helper()
	layout, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create layout: %v", err)
	}
	return layout.BPFFS()
}

func TestSnapshot_ManagedPrograms(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{
		programs: map[kernel.ProgramID]bpfman.ProgramRecord{
			100: {ProgramID: 100, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeXDP), Handles: bpfman.ProgramHandles{PinPath: "/run/bpfman/fs/prog_100"}, Meta: bpfman.ProgramMeta{Name: "xdp_pass"}},
			200: {ProgramID: 200, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeTC), Handles: bpfman.ProgramHandles{PinPath: "/run/bpfman/fs/prog_200"}, Meta: bpfman.ProgramMeta{Name: "tc_filter"}},
		},
	}

	kern := &fakeKernelSource{
		programs: []kernel.Program{
			{ID: 100},
			{ID: 200},
		},
	}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	managed := w.ManagedPrograms()
	assert.Len(t, managed, 2)

	// Verify all managed programs are in store
	for _, p := range managed {
		assert.True(t, p.Presence.InStore)
		assert.True(t, p.Presence.InKernel)
	}
}

func TestSnapshot_KernelOnlyPrograms(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{
		programs: map[kernel.ProgramID]bpfman.ProgramRecord{
			100: {ProgramID: 100, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeXDP), Meta: bpfman.ProgramMeta{Name: "managed"}},
		},
	}

	kern := &fakeKernelSource{
		programs: []kernel.Program{
			{ID: 100}, // managed
			{ID: 999}, // kernel-only
		},
	}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	// All programs (managed + kernel-only)
	assert.Len(t, w.Programs, 2)

	// Only managed
	managed := w.ManagedPrograms()
	assert.Len(t, managed, 1)
	assert.Equal(t, kernel.ProgramID(100), managed[0].ProgramID)

	// Find kernel-only
	var kernelOnly *inspect.ProgramView
	for i := range w.Programs {
		if w.Programs[i].Presence.KernelOnly() {
			kernelOnly = &w.Programs[i]
			break
		}
	}
	require.NotNil(t, kernelOnly)
	assert.Equal(t, kernel.ProgramID(999), kernelOnly.ProgramID)
	assert.False(t, kernelOnly.Presence.InStore)
	assert.True(t, kernelOnly.Presence.InKernel)
}

func TestSnapshot_FSOnlyPrograms(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)

	// Create an orphan prog pin on FS
	require.NoError(t, os.MkdirAll(bpfFS.MountPoint(), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bpfFS.MountPoint(), "prog_888"), nil, 0644))

	scanner := bpfFS.Scanner()
	store := &fakeStore{programs: map[kernel.ProgramID]bpfman.ProgramRecord{}}
	kern := &fakeKernelSource{}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	assert.Len(t, w.Programs, 1)
	assert.Equal(t, kernel.ProgramID(888), w.Programs[0].ProgramID)
	assert.True(t, w.Programs[0].Presence.OrphanFS())
	assert.False(t, w.Programs[0].Presence.InStore)
	assert.False(t, w.Programs[0].Presence.InKernel)
	assert.True(t, w.Programs[0].Presence.InFS)
}

func TestSnapshot_Links(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{
		links: []bpfman.LinkRecord{
			// ID is now the kernel link ID for non-synthetic links
			{ID: kernel.LinkID(10), Kind: bpfman.LinkKindXDP},
			{ID: kernel.LinkID(20), Kind: bpfman.LinkKindKprobe},
		},
	}

	kern := &fakeKernelSource{
		links: []kernel.Link{
			{ID: 10, ProgramID: 100},
			{ID: 20, ProgramID: 200},
			{ID: 999}, // kernel-only link
		},
	}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	assert.Len(t, w.Links, 3)

	managed := w.ManagedLinks()
	assert.Len(t, managed, 2)

	// Check kernel-only link
	var kernelOnly *inspect.LinkRow
	for i := range w.Links {
		if w.Links[i].Presence.KernelOnly() {
			kernelOnly = &w.Links[i]
			break
		}
	}
	require.NotNil(t, kernelOnly)
	require.NotNil(t, kernelOnly.Kernel)
	assert.Equal(t, kernel.LinkID(999), kernelOnly.Kernel.ID)
}

func TestSnapshot_Dispatchers(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	require.NoError(t, os.MkdirAll(bpfFS.XDP(), 0755))

	// Create dispatcher dir on FS
	dispDir := filepath.Join(bpfFS.XDP(), "dispatcher_1_1_5")
	require.NoError(t, os.Mkdir(dispDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dispDir, "link_0"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dispDir, "link_1"), nil, 0644))

	scanner := bpfFS.Scanner()

	linkID := kernel.LinkID(50)
	store := &fakeStore{
		dispatchers: []platform.DispatcherSummary{
			{
				Key:      dispatcher.Key{Type: dispatcher.DispatcherTypeXDP, Nsid: 1, Ifindex: 1},
				Revision: 5,
				Runtime: platform.DispatcherRuntime{
					ProgramID: 500,
					LinkID:    &linkID,
				},
			},
		},
	}

	kern := &fakeKernelSource{
		programs: []kernel.Program{{ID: 500}},
		links:    []kernel.Link{{ID: 50}},
	}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	assert.Len(t, w.Dispatchers, 1)

	d := w.Dispatchers[0]
	assert.Equal(t, "xdp", d.DispType)
	assert.Equal(t, uint64(1), d.Nsid)
	assert.Equal(t, uint32(1), d.Ifindex)
	assert.Equal(t, uint32(5), d.Revision)
	assert.Equal(t, 2, d.FSLinkCount)
	assert.True(t, d.ProgPresence.InStore)
	assert.True(t, d.ProgPresence.InKernel)
	assert.True(t, d.ProgPresence.InFS)
	require.NotNil(t, d.Managed, "store dispatcher should have Managed set")
	assert.Equal(t, kernel.ProgramID(500), d.Managed.Runtime.ProgramID)
}

func TestSnapshot_OrphanDispatcher(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	require.NoError(t, os.MkdirAll(bpfFS.XDP(), 0755))

	// Create orphan dispatcher dir on FS (not in store)
	dispDir := filepath.Join(bpfFS.XDP(), "dispatcher_99_2_1")
	require.NoError(t, os.Mkdir(dispDir, 0755))

	scanner := bpfFS.Scanner()
	store := &fakeStore{}
	kern := &fakeKernelSource{}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	assert.Len(t, w.Dispatchers, 1)

	d := w.Dispatchers[0]
	assert.Equal(t, "xdp", d.DispType)
	assert.Equal(t, uint64(99), d.Nsid)
	assert.Equal(t, uint32(2), d.Ifindex)
	assert.False(t, d.ProgPresence.InStore)
	assert.False(t, d.ProgPresence.InKernel)
	assert.True(t, d.ProgPresence.InFS)
	assert.Nil(t, d.Managed, "orphan dispatcher should have nil Managed")
}

func TestPresence_Methods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		p          inspect.Presence
		managed    bool
		orphanFS   bool
		kernelOnly bool
	}{
		{
			name:       "in store only",
			p:          inspect.Presence{InStore: true, InKernel: false, InFS: false},
			managed:    true,
			orphanFS:   false,
			kernelOnly: false,
		},
		{
			name:       "fully present",
			p:          inspect.Presence{InStore: true, InKernel: true, InFS: true},
			managed:    true,
			orphanFS:   false,
			kernelOnly: false,
		},
		{
			name:       "kernel only",
			p:          inspect.Presence{InStore: false, InKernel: true, InFS: false},
			managed:    false,
			orphanFS:   false,
			kernelOnly: true,
		},
		{
			name:       "kernel and fs, not store",
			p:          inspect.Presence{InStore: false, InKernel: true, InFS: true},
			managed:    false,
			orphanFS:   false,
			kernelOnly: true,
		},
		{
			name:       "fs only (orphan)",
			p:          inspect.Presence{InStore: false, InKernel: false, InFS: true},
			managed:    false,
			orphanFS:   true,
			kernelOnly: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.managed, tt.p.Managed())
			assert.Equal(t, tt.orphanFS, tt.p.OrphanFS())
			assert.Equal(t, tt.kernelOnly, tt.p.KernelOnly())
		})
	}
}

func TestGetProgram_FullyPresent(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)

	// Create a pin file on FS
	require.NoError(t, os.MkdirAll(bpfFS.MountPoint(), 0755))
	pinPath := filepath.Join(bpfFS.MountPoint(), "prog_100")
	require.NoError(t, os.WriteFile(pinPath, nil, 0644))

	scanner := bpfFS.Scanner()

	store := &fakeStore{
		programs: map[kernel.ProgramID]bpfman.ProgramRecord{
			100: {ProgramID: 100, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeXDP), Handles: bpfman.ProgramHandles{PinPath: pinPath}, Meta: bpfman.ProgramMeta{Name: "xdp_pass"}},
		},
	}

	kern := &fakeKernelSource{
		programs: []kernel.Program{{ID: 100, Name: "xdp_pass"}},
	}

	row, err := inspect.GetProgram(context.Background(), store, kern, scanner, 100)
	require.NoError(t, err)

	assert.Equal(t, kernel.ProgramID(100), row.ProgramID)
	assert.True(t, row.Presence.InStore)
	assert.True(t, row.Presence.InKernel)
	assert.True(t, row.Presence.InFS)
	assert.NotNil(t, row.Managed)
	assert.NotNil(t, row.Kernel)
	assert.Equal(t, "xdp_pass", row.Managed.Meta.Name)
	assert.Equal(t, "xdp_pass", row.Kernel.Name)
}

func TestGetProgram_StoreOnly(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{
		programs: map[kernel.ProgramID]bpfman.ProgramRecord{
			100: {ProgramID: 100, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeXDP), Meta: bpfman.ProgramMeta{Name: "stale_prog"}},
		},
	}

	kern := &fakeKernelSource{} // Program not in kernel

	row, err := inspect.GetProgram(context.Background(), store, kern, scanner, 100)
	require.NoError(t, err)

	assert.Equal(t, kernel.ProgramID(100), row.ProgramID)
	assert.True(t, row.Presence.InStore)
	assert.False(t, row.Presence.InKernel)
	assert.False(t, row.Presence.InFS)
	assert.NotNil(t, row.Managed)
	assert.Nil(t, row.Kernel)
}

func TestGetProgram_KernelOnly(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{programs: map[kernel.ProgramID]bpfman.ProgramRecord{}} // Not in store

	kern := &fakeKernelSource{
		programs: []kernel.Program{{ID: 999, Name: "unmanaged"}},
	}

	row, err := inspect.GetProgram(context.Background(), store, kern, scanner, 999)
	require.NoError(t, err)

	assert.Equal(t, kernel.ProgramID(999), row.ProgramID)
	assert.False(t, row.Presence.InStore)
	assert.True(t, row.Presence.InKernel)
	assert.False(t, row.Presence.InFS)
	assert.Nil(t, row.Managed)
	assert.NotNil(t, row.Kernel)
	assert.Equal(t, "unmanaged", row.Kernel.Name)
}

func TestGetProgram_NotFound(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{programs: map[kernel.ProgramID]bpfman.ProgramRecord{}}
	kern := &fakeKernelSource{}

	_, err := inspect.GetProgram(context.Background(), store, kern, scanner, 12345)
	require.Error(t, err)
	assert.ErrorIs(t, err, inspect.ErrNotFound)
}

func TestGetLink_FullyPresent(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)

	// Create a pin file on FS
	pinPath := filepath.Join(bpfFS.Links(), "100", "link_10")
	require.NoError(t, os.MkdirAll(filepath.Dir(pinPath), 0755))
	require.NoError(t, os.WriteFile(pinPath, nil, 0644))

	scanner := bpfFS.Scanner()

	// ID is now the kernel link ID for non-synthetic links
	store := &fakeStore{
		links: []bpfman.LinkRecord{
			{
				ID:      kernel.LinkID(10), // kernel link ID
				Kind:    bpfman.LinkKindKprobe,
				PinPath: bpfman.NewLinkPath(pinPath),
				Details: bpfman.KprobeDetails{FnName: "do_sys_open"},
			},
		},
	}

	kern := &fakeKernelSource{
		links: []kernel.Link{{ID: 10, ProgramID: 100}},
	}

	info, err := inspect.GetLink(context.Background(), store, kern, scanner, 10) // LinkID 10 (same as kernel link ID)
	require.NoError(t, err)

	assert.Equal(t, kernel.LinkID(10), info.Record.ID)
	assert.True(t, info.Presence.InStore)
	assert.True(t, info.Presence.InKernel)
	assert.True(t, info.Presence.InFS)
	assert.NotNil(t, info.Record.Details)
}

func TestGetLink_StoreOnly(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	// ID is now the kernel link ID for non-synthetic links
	store := &fakeStore{
		links: []bpfman.LinkRecord{
			{ID: kernel.LinkID(20), Kind: bpfman.LinkKindTracepoint},
		},
	}

	kern := &fakeKernelSource{} // Link not in kernel

	info, err := inspect.GetLink(context.Background(), store, kern, scanner, 20) // LinkID 20 (same as kernel link ID)
	require.NoError(t, err)

	assert.Equal(t, kernel.LinkID(20), info.Record.ID)
	assert.True(t, info.Presence.InStore)
	assert.False(t, info.Presence.InKernel)
	assert.False(t, info.Presence.InFS)
}

func TestGetLink_NotInStore(t *testing.T) {
	t.Parallel()

	// GetLink requires the link to be in the store (it takes a durable LinkID).
	// If the link is not in the store, it returns inspect.ErrNotFound.
	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{} // Not in store

	kern := &fakeKernelSource{
		links: []kernel.Link{{ID: 999}},
	}

	// Even though link 999 exists in kernel, we can't look it up by LinkID 999
	// because LinkID is a store-assigned durable ID, not a kernel link ID.
	_, err := inspect.GetLink(context.Background(), store, kern, scanner, 999)
	require.Error(t, err)
	assert.ErrorIs(t, err, inspect.ErrNotFound)
}

func TestGetLink_NotFound(t *testing.T) {
	t.Parallel()

	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{}
	kern := &fakeKernelSource{}

	_, err := inspect.GetLink(context.Background(), store, kern, scanner, 12345)
	require.Error(t, err)
	assert.ErrorIs(t, err, inspect.ErrNotFound)
}

func TestSnapshot_LinksHaveDetails(t *testing.T) {
	t.Parallel()

	// Verify that Snapshot() returns a World where links have Details populated.
	// This is critical for the ATTACH column in CLI output.
	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	// Create links WITH details populated (simulating what the real store returns)
	store := &fakeStore{
		programs: map[kernel.ProgramID]bpfman.ProgramRecord{
			100: {ProgramID: 100, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeTracepoint), Meta: bpfman.ProgramMeta{Name: "test_prog"}},
		},
		links: []bpfman.LinkRecord{
			{
				ID:        kernel.LinkID(10),
				Kind:      bpfman.LinkKindTracepoint,
				ProgramID: 100,
				Details:   bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
			},
			{
				ID:        kernel.LinkID(20),
				Kind:      bpfman.LinkKindKprobe,
				ProgramID: 100,
				Details:   bpfman.KprobeDetails{FnName: "do_sys_open"},
			},
		},
	}

	kern := &fakeKernelSource{
		programs: []kernel.Program{{ID: 100}},
		links: []kernel.Link{
			{ID: 10, ProgramID: 100},
			{ID: 20, ProgramID: 100},
		},
	}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	// Verify links in World have details
	managed := w.ManagedLinks()
	require.Len(t, managed, 2)

	for _, linkRow := range managed {
		require.NotNil(t, linkRow.Managed, "Managed should not be nil")
		require.NotNil(t, linkRow.Managed.Details, "link %d Details should not be nil", linkRow.ID())
	}

	// Verify details are correct types
	linksByID := make(map[kernel.LinkID]inspect.LinkRow)
	for _, l := range managed {
		linksByID[l.ID()] = l
	}

	tpLink := linksByID[kernel.LinkID(10)]
	tpDetails, ok := tpLink.Managed.Details.(bpfman.TracepointDetails)
	require.True(t, ok, "expected TracepointDetails")
	assert.Equal(t, "sched", tpDetails.Group)
	assert.Equal(t, "sched_switch", tpDetails.Name)

	kpLink := linksByID[kernel.LinkID(20)]
	kpDetails, ok := kpLink.Managed.Details.(bpfman.KprobeDetails)
	require.True(t, ok, "expected KprobeDetails")
	assert.Equal(t, "do_sys_open", kpDetails.FnName)
}

func TestSnapshot_ProgramLinksHaveDetails(t *testing.T) {
	t.Parallel()

	// Verify that links correlated to programs also have details populated.
	bpfFS := testBPFFS(t)
	scanner := bpfFS.Scanner()

	store := &fakeStore{
		programs: map[kernel.ProgramID]bpfman.ProgramRecord{
			100: {ProgramID: 100, Load: bpfman.TestLoadSpec(bpfman.ProgramTypeTracepoint), Meta: bpfman.ProgramMeta{Name: "test_prog"}},
		},
		links: []bpfman.LinkRecord{
			{
				ID:        kernel.LinkID(10),
				Kind:      bpfman.LinkKindTracepoint,
				ProgramID: 100,
				Details:   bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
			},
		},
	}

	kern := &fakeKernelSource{
		programs: []kernel.Program{{ID: 100}},
		links:    []kernel.Link{{ID: 10, ProgramID: 100}},
	}

	w, err := inspect.Snapshot(context.Background(), store, kern, scanner)
	require.NoError(t, err)

	// Find the program and verify its correlated links have details
	managed := w.ManagedPrograms()
	require.Len(t, managed, 1)

	prog := managed[0]
	require.Len(t, prog.Links, 1, "program should have 1 correlated link")

	linkRow := prog.Links[0]
	require.NotNil(t, linkRow.Managed)
	require.NotNil(t, linkRow.Managed.Details, "correlated link Details should not be nil")

	tpDetails, ok := linkRow.Managed.Details.(bpfman.TracepointDetails)
	require.True(t, ok)
	assert.Equal(t, "sched", tpDetails.Group)
	assert.Equal(t, "sched_switch", tpDetails.Name)
}
