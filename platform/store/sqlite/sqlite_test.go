package sqlite_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
)

// testLogger returns a logger for tests. By default it discards all output.
// Set BPFMAN_TEST_VERBOSE=1 to enable logging.
func testLogger() *slog.Logger {
	if os.Getenv("BPFMAN_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testProgram returns a valid ProgramRecord for testing.
func testProgram() bpfman.ProgramRecord {
	return bpfman.ProgramRecord{
		Load: bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeTracepoint, "/test/path/program.o"),
		Handles: bpfman.ProgramHandles{
			PinPath: "/sys/fs/bpf/test",
		},
		Meta: bpfman.ProgramMeta{
			Name: "test_program",
		},
		CreatedAt: time.Now(),
	}
}

func TestForeignKey_LinkRequiresProgram(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Attempt to create a link referencing a non-existent program.
	details := bpfman.TracepointDetails{
		Group: "syscalls",
		Name:  "sys_enter_openat",
	}
	linkID := kernel.LinkID(1)
	spec := bpfman.NewEphemeralLinkRecord(linkID, kernel.ProgramID(999), details, time.Now()) // program 999 does not exist

	err = store.SaveLink(ctx, spec)
	require.Error(t, err, "expected FK constraint violation")
	assert.True(t, strings.Contains(err.Error(), "FOREIGN KEY constraint failed"), "expected FK constraint error, got: %v", err)
}

func TestForeignKey_CascadeDeleteRemovesLinks(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program directly.
	kernelID := kernel.ProgramID(42)
	prog := testProgram()

	require.NoError(t, store.Save(ctx, kernelID, prog), "Save failed")

	// Create two links for that program.
	for i := 0; i < 2; i++ {
		details := bpfman.KprobeDetails{
			FnName:   "test_fn",
			Offset:   0,
			Retprobe: false,
		}
		linkID := kernel.LinkID(100 + i)
		spec := bpfman.NewEphemeralLinkRecord(linkID, kernelID, details, time.Now())
		err := store.SaveLink(ctx, spec)
		require.NoError(t, err, "SaveLink failed")
	}

	// Verify links exist.
	links, err := store.ListLinksByProgram(ctx, kernelID)
	require.NoError(t, err, "ListLinksByProgram failed")
	require.Len(t, links, 2, "expected 2 links")

	// Delete the program.
	require.NoError(t, store.Delete(ctx, kernelID), "Delete failed")

	// Verify CASCADE removed the links.
	links, err = store.ListLinksByProgram(ctx, kernelID)
	require.NoError(t, err, "ListLinksByProgram after delete failed")
	assert.Empty(t, links, "expected 0 links after CASCADE delete")
}

func TestMetadata_StoredAsJSON(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program with metadata.
	kernelID := kernel.ProgramID(42)
	prog := testProgram()
	prog.Meta.Metadata = map[string]string{
		"app":     "test",
		"version": "1.0",
	}

	require.NoError(t, store.Save(ctx, kernelID, prog), "Save failed")

	// Verify metadata is stored and retrieved correctly.
	found, err := store.Get(ctx, kernelID)
	require.NoError(t, err, "Get failed")
	assert.Equal(t, "test", found.Meta.Metadata["app"], "metadata app mismatch")
	assert.Equal(t, "1.0", found.Meta.Metadata["version"], "metadata version mismatch")

	// Delete the program.
	require.NoError(t, store.Delete(ctx, kernelID), "Delete failed")

	// Verify program is gone.
	_, err = store.Get(ctx, kernelID)
	assert.Error(t, err, "expected error after delete")
}

func TestProgramName_DuplicatesAllowed(t *testing.T) {
	// Multiple programs can share the same bpfman.io/ProgramName, e.g., when
	// loading multiple BPF programs from a single OCI image via the operator.
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create first program with a name.
	prog1 := testProgram()
	prog1.Meta.Metadata = map[string]string{
		"bpfman.io/ProgramName": "my-program",
	}

	require.NoError(t, store.Save(ctx, kernel.ProgramID(100), prog1), "Save prog1 failed")

	// Create second program with the same name - this should succeed.
	prog2 := testProgram()
	prog2.Meta.Metadata = map[string]string{
		"bpfman.io/ProgramName": "my-program", // same name, allowed
	}

	err = store.Save(ctx, kernel.ProgramID(200), prog2)
	require.NoError(t, err, "duplicate program names should be allowed")
}

func TestUniqueIndex_DifferentNamesAllowed(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create two programs with different names.
	for i, name := range []string{"program-a", "program-b"} {
		prog := testProgram()
		prog.Meta.Metadata = map[string]string{
			"bpfman.io/ProgramName": name,
		}

		require.NoError(t, store.Save(ctx, kernel.ProgramID(100+i), prog), "Save %s failed", name)
	}

	// Verify both exist.
	programs, err := store.List(ctx)
	require.NoError(t, err, "List failed")
	assert.Len(t, programs, 2, "expected 2 programs")
}

func TestUniqueIndex_NameCanBeReusedAfterDelete(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program with a name.
	prog := testProgram()
	prog.Meta.Metadata = map[string]string{
		"bpfman.io/ProgramName": "reusable-name",
	}

	require.NoError(t, store.Save(ctx, kernel.ProgramID(100), prog), "Save failed")

	// Delete it.
	require.NoError(t, store.Delete(ctx, kernel.ProgramID(100)), "Delete failed")

	// Create a new program with the same name.
	prog2 := testProgram()
	prog2.Meta.Metadata = map[string]string{
		"bpfman.io/ProgramName": "reusable-name", // same name, should work
	}

	require.NoError(t, store.Save(ctx, kernel.ProgramID(200), prog2), "Save prog2 failed")

	// Verify it exists.
	found, err := store.Get(ctx, kernel.ProgramID(200))
	require.NoError(t, err, "Get failed")
	assert.Equal(t, "reusable-name", found.Meta.Metadata["bpfman.io/ProgramName"], "name mismatch")
}

func TestLinkRegistry_TracepointRoundTrip(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program first
	prog := testProgram()
	require.NoError(t, store.Save(ctx, kernel.ProgramID(42), prog), "Save failed")

	// Create a tracepoint link
	linkID := kernel.LinkID(100)
	details := bpfman.TracepointDetails{
		Group: "syscalls",
		Name:  "sys_enter_openat",
	}
	spec := bpfman.NewPinnedLinkRecord(linkID, kernel.ProgramID(42), details, bpffs.LinkPath("/sys/fs/bpf/bpfman/test/link"), time.Now())

	err = store.SaveLink(ctx, spec)
	require.NoError(t, err, "SaveLink failed")

	// Retrieve and verify
	gotSpec, err := store.GetLink(ctx, linkID)
	require.NoError(t, err, "GetLink failed")

	assert.Equal(t, bpfman.LinkKindTracepoint, gotSpec.Kind)
	assert.Equal(t, linkID, gotSpec.ID)
	assert.Equal(t, kernel.ProgramID(42), gotSpec.ProgramID, "ProgramID should match the program kernel ID passed to SaveLink")
	assert.Equal(t, spec.PinPath, gotSpec.PinPath)

	tpDetails, ok := gotSpec.Details.(bpfman.TracepointDetails)
	require.True(t, ok, "expected TracepointDetails")
	assert.Equal(t, details.Group, tpDetails.Group)
	assert.Equal(t, details.Name, tpDetails.Name)
}

func TestLinkRegistry_LinkIDUniqueness(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program first
	prog := testProgram()
	require.NoError(t, store.Save(ctx, kernel.ProgramID(42), prog), "Save failed")

	// Create first link
	linkID := kernel.LinkID(100)
	details := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_enter_openat"}
	spec := bpfman.NewEphemeralLinkRecord(linkID, kernel.ProgramID(42), details, time.Now())

	err = store.SaveLink(ctx, spec)
	require.NoError(t, err, "first SaveLink failed")

	// Try to create another link with same link_id (primary key violation)
	kprobeDetails := bpfman.KprobeDetails{FnName: "test_fn"}
	spec2 := bpfman.NewEphemeralLinkRecord(linkID, kernel.ProgramID(42), kprobeDetails, time.Now())

	err = store.SaveLink(ctx, spec2) // same link_id
	require.Error(t, err, "expected link_id uniqueness violation")
	assert.True(t, strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY"),
		"expected uniqueness error, got: %v", err)
}

func TestLinkRegistry_CascadeDeleteFromRegistry(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program first
	prog := testProgram()
	require.NoError(t, store.Save(ctx, kernel.ProgramID(42), prog), "Save failed")

	// Create a tracepoint link
	linkID := kernel.LinkID(100)
	details := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_enter_openat"}
	spec := bpfman.NewEphemeralLinkRecord(linkID, kernel.ProgramID(42), details, time.Now())

	err = store.SaveLink(ctx, spec)
	require.NoError(t, err, "SaveLink failed")

	// Delete the link via registry
	require.NoError(t, store.DeleteLink(ctx, linkID), "DeleteLink failed")

	// Verify link is gone
	_, err = store.GetLink(ctx, linkID)
	require.Error(t, err, "expected link to be deleted")
}

// ----------------------------------------------------------------------------
// Dispatcher Store Tests
// ----------------------------------------------------------------------------

func TestDispatcherStore_SaveAndGet(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a dispatcher
	state := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  1,
		Revision: 1,
		KernelID: 100,
		LinkID:   101,
	}

	require.NoError(t, store.SaveDispatcher(ctx, state), "SaveDispatcher failed")

	// Retrieve and verify
	got, err := store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "GetDispatcher failed")

	assert.Equal(t, state.Type, got.Type)
	assert.Equal(t, state.Nsid, got.Nsid)
	assert.Equal(t, state.Ifindex, got.Ifindex)
	assert.Equal(t, state.Revision, got.Revision)
	assert.Equal(t, state.KernelID, got.KernelID)
	assert.Equal(t, state.LinkID, got.LinkID)
}

func TestDispatcherStore_Update(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a dispatcher
	state := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  1,
		Revision: 1,
		KernelID: 100,
		LinkID:   101,
	}

	require.NoError(t, store.SaveDispatcher(ctx, state), "SaveDispatcher failed")

	// Update it
	state.Revision = 2

	require.NoError(t, store.SaveDispatcher(ctx, state), "SaveDispatcher (update) failed")

	// Verify the update
	got, err := store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "GetDispatcher failed")

	assert.Equal(t, uint32(2), got.Revision)
}

func TestDispatcherStore_Delete(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a dispatcher
	state := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  1,
		Revision: 1,
		KernelID: 100,
		LinkID:   101,
	}

	require.NoError(t, store.SaveDispatcher(ctx, state), "SaveDispatcher failed")

	// Delete it
	require.NoError(t, store.DeleteDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1), "DeleteDispatcher failed")

	// Verify it's gone
	_, err = store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.Error(t, err, "expected dispatcher to be deleted")
}

func TestDispatcherStore_DeleteNonExistent(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Try to delete a non-existent dispatcher
	err = store.DeleteDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 99)
	require.Error(t, err, "expected error for non-existent dispatcher")
}

func TestDispatcherStore_IncrementRevision(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a dispatcher with revision 1
	state := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  1,
		Revision: 1,
		KernelID: 100,
		LinkID:   101,
	}

	require.NoError(t, store.SaveDispatcher(ctx, state), "SaveDispatcher failed")

	// Increment revision
	newRev, err := store.IncrementRevision(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "IncrementRevision failed")
	assert.Equal(t, uint32(2), newRev, "expected revision 2")

	// Increment again
	newRev, err = store.IncrementRevision(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "IncrementRevision (2nd) failed")
	assert.Equal(t, uint32(3), newRev, "expected revision 3")

	// Verify via Get
	got, err := store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "GetDispatcher failed")
	assert.Equal(t, uint32(3), got.Revision, "revision mismatch")
}

func TestDispatcherStore_UniqueConstraint(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create an XDP dispatcher
	xdpState := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  1,
		Revision: 1,
		KernelID: 100,
		LinkID:   101,
	}

	require.NoError(t, store.SaveDispatcher(ctx, xdpState), "SaveDispatcher (xdp) failed")

	// Create a TC-ingress dispatcher on same nsid/ifindex - should work (different type)
	// TC dispatchers have LinkID=0 (they use netlink filters, not BPF links)
	tcState := dispatcher.State{
		Type:     dispatcher.DispatcherTypeTCIngress,
		Nsid:     4026531840,
		Ifindex:  1,
		Revision: 1,
		KernelID: 200,
		LinkID:   0,
	}

	require.NoError(t, store.SaveDispatcher(ctx, tcState), "SaveDispatcher (tc-ingress) failed")

	// Verify both exist
	_, err = store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "GetDispatcher (xdp) failed")

	_, err = store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeTCIngress), 4026531840, 1)
	require.NoError(t, err, "GetDispatcher (tc-ingress) failed")
}

func TestDispatcherStore_DifferentInterfaces(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create dispatchers for ifindex 1 and 2
	for ifindex := uint32(1); ifindex <= 2; ifindex++ {
		state := dispatcher.State{
			Type:     dispatcher.DispatcherTypeXDP,
			Nsid:     4026531840,
			Ifindex:  ifindex,
			Revision: 1,
			KernelID: kernel.ProgramID(100 + ifindex),
			LinkID:   kernel.LinkID(200 + ifindex),
		}
		require.NoError(t, store.SaveDispatcher(ctx, state), "SaveDispatcher (ifindex %d) failed", ifindex)
	}

	// Verify both exist independently
	got1, err := store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 1)
	require.NoError(t, err, "GetDispatcher (ifindex 1) failed")
	assert.Equal(t, kernel.ProgramID(101), got1.KernelID)

	got2, err := store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), 4026531840, 2)
	require.NoError(t, err, "GetDispatcher (ifindex 2) failed")
	assert.Equal(t, kernel.ProgramID(102), got2.KernelID)
}

// ----------------------------------------------------------------------------
// Map Ownership Tests
// ----------------------------------------------------------------------------

func TestMapOwnership_CountDependentPrograms(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create the owner program (first program from an image).
	ownerID := kernel.ProgramID(100)
	ownerProg := testProgram()
	ownerProg.Meta.Name = "kprobe_counter"
	ownerProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100"
	require.NoError(t, store.Save(ctx, ownerID, ownerProg), "Save owner failed")

	// Initially no dependents.
	count, err := store.CountDependentPrograms(ctx, ownerID)
	require.NoError(t, err, "CountDependentPrograms failed")
	assert.Equal(t, 0, count, "expected 0 dependents initially")

	// Create dependent programs that share the owner's maps.
	for i := kernel.ProgramID(1); i <= 3; i++ {
		depProg := testProgram()
		depProg.Meta.Name = "dependent_" + string(rune('0'+i))
		depProg.Handles.MapOwnerID = &ownerID
		depProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100" // Same as owner
		require.NoError(t, store.Save(ctx, 100+i, depProg), "Save dependent %d failed", i)
	}

	// Now we should have 3 dependents.
	count, err = store.CountDependentPrograms(ctx, ownerID)
	require.NoError(t, err, "CountDependentPrograms failed")
	assert.Equal(t, 3, count, "expected 3 dependents")

	// Delete one dependent.
	require.NoError(t, store.Delete(ctx, kernel.ProgramID(101)), "Delete dependent failed")

	// Now we should have 2 dependents.
	count, err = store.CountDependentPrograms(ctx, ownerID)
	require.NoError(t, err, "CountDependentPrograms failed")
	assert.Equal(t, 2, count, "expected 2 dependents after delete")
}

func TestMapOwnership_ForeignKeyPreventsDeletingOwner(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create the owner program.
	ownerID := kernel.ProgramID(100)
	ownerProg := testProgram()
	ownerProg.Meta.Name = "owner"
	ownerProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100"
	require.NoError(t, store.Save(ctx, ownerID, ownerProg), "Save owner failed")

	// Create a dependent program.
	depProg := testProgram()
	depProg.Meta.Name = "dependent"
	depProg.Handles.MapOwnerID = &ownerID
	depProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100"
	require.NoError(t, store.Save(ctx, kernel.ProgramID(101), depProg), "Save dependent failed")

	// Attempt to delete the owner while dependent exists - should fail due to FK.
	err = store.Delete(ctx, ownerID)
	require.Error(t, err, "expected FK constraint violation when deleting owner")
	assert.Contains(t, err.Error(), "FOREIGN KEY constraint failed",
		"expected FK constraint error, got: %v", err)

	// Delete the dependent first.
	require.NoError(t, store.Delete(ctx, kernel.ProgramID(101)), "Delete dependent failed")

	// Now we can delete the owner.
	require.NoError(t, store.Delete(ctx, ownerID), "Delete owner failed after dependents removed")
}

func TestMapOwnership_MapPinPathPersisted(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program with MapPinPath set.
	kernelID := kernel.ProgramID(42)
	prog := testProgram()
	prog.Handles.MapPinPath = "/sys/fs/bpf/bpfman/42"

	require.NoError(t, store.Save(ctx, kernelID, prog), "Save failed")

	// Retrieve and verify MapPinPath is persisted.
	got, err := store.Get(ctx, kernelID)
	require.NoError(t, err, "Get failed")
	assert.Equal(t, "/sys/fs/bpf/bpfman/42", got.Handles.MapPinPath, "MapPinPath mismatch")
}

func TestMapOwnership_MapOwnerIDPersisted(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create the owner program first.
	ownerID := kernel.ProgramID(100)
	ownerProg := testProgram()
	ownerProg.Meta.Name = "owner"
	require.NoError(t, store.Save(ctx, ownerID, ownerProg), "Save owner failed")

	// Create a dependent program with MapOwnerID set.
	depID := kernel.ProgramID(101)
	depProg := testProgram()
	depProg.Meta.Name = "dependent"
	depProg.Handles.MapOwnerID = &ownerID
	depProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100"

	require.NoError(t, store.Save(ctx, depID, depProg), "Save dependent failed")

	// Retrieve and verify MapOwnerID is persisted.
	got, err := store.Get(ctx, depID)
	require.NoError(t, err, "Get failed")
	require.NotNil(t, got.Handles.MapOwnerID, "MapOwnerID should not be nil")
	assert.Equal(t, ownerID, *got.Handles.MapOwnerID, "MapOwnerID mismatch")
}

func TestMapOwnership_ListIncludesMapFields(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create owner.
	ownerID := kernel.ProgramID(100)
	ownerProg := testProgram()
	ownerProg.Meta.Name = "owner"
	ownerProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100"
	require.NoError(t, store.Save(ctx, ownerID, ownerProg), "Save owner failed")

	// Create dependent.
	depID := kernel.ProgramID(101)
	depProg := testProgram()
	depProg.Meta.Name = "dependent"
	depProg.Handles.MapOwnerID = &ownerID
	depProg.Handles.MapPinPath = "/sys/fs/bpf/bpfman/100"
	require.NoError(t, store.Save(ctx, depID, depProg), "Save dependent failed")

	// List all programs.
	programs, err := store.List(ctx)
	require.NoError(t, err, "List failed")
	require.Len(t, programs, 2, "expected 2 programs")

	// Verify owner has MapPinPath but no MapOwnerID.
	owner := programs[ownerID]
	assert.Equal(t, "/sys/fs/bpf/bpfman/100", owner.Handles.MapPinPath, "owner MapPinPath mismatch")
	assert.Nil(t, owner.Handles.MapOwnerID, "owner should have no MapOwnerID")

	// Verify dependent has both fields.
	dep := programs[depID]
	assert.Equal(t, "/sys/fs/bpf/bpfman/100", dep.Handles.MapPinPath, "dependent MapPinPath mismatch")
	require.NotNil(t, dep.Handles.MapOwnerID, "dependent should have MapOwnerID set")
	assert.Equal(t, ownerID, *dep.Handles.MapOwnerID, "dependent MapOwnerID mismatch")
}

// TestListTCXLinksByInterface_OrderByPriority verifies that TCX links are
// returned in priority order (ascending), which is critical for correctly
// computing attach order when inserting new TCX programs.
func TestListTCXLinksByInterface_OrderByPriority(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program for the links to reference.
	progID := kernel.ProgramID(100)
	prog := testProgram()
	prog.Load = bpfman.TestLoadSpec(bpfman.ProgramTypeTCX)
	require.NoError(t, store.Save(ctx, progID, prog), "Save program failed")

	// Create TCX links with varying priorities (insert out of order).
	const (
		nsid      = uint64(4026531840)
		ifindex   = uint32(2)
		direction = "ingress"
	)

	// Insert links with priorities: 300, 100, 500, 200 (intentionally unordered)
	linksToCreate := []struct {
		linkID   uint32
		priority int32
	}{
		{linkID: 1001, priority: 300},
		{linkID: 1002, priority: 100},
		{linkID: 1003, priority: 500},
		{linkID: 1004, priority: 200},
	}

	for _, link := range linksToCreate {
		details := bpfman.TCXDetails{
			Interface: "eth0",
			Ifindex:   ifindex,
			Direction: direction,
			Priority:  link.priority,
			Nsid:      nsid,
		}
		linkID := kernel.LinkID(link.linkID)
		spec := bpfman.NewPinnedLinkRecord(linkID, progID, details, bpffs.LinkPath("/sys/fs/bpf/link_"+string(rune(link.linkID))), time.Now())
		err := store.SaveLink(ctx, spec)
		require.NoError(t, err, "SaveLink failed for link %d", link.linkID)
	}

	// Query links - they should be ordered by priority ASC.
	links, err := store.ListTCXLinksByInterface(ctx, nsid, ifindex, direction)
	require.NoError(t, err, "ListTCXLinksByInterface failed")
	require.Len(t, links, 4, "expected 4 links")

	// Verify order: priorities should be 100, 200, 300, 500
	expectedPriorities := []int32{100, 200, 300, 500}
	for i, link := range links {
		assert.Equal(t, expectedPriorities[i], link.Priority,
			"link at position %d has wrong priority", i)
	}

	// Verify the correct kernel link IDs are in order
	expectedKernelLinkIDs := []kernel.LinkID{1002, 1004, 1001, 1003}
	for i, link := range links {
		assert.Equal(t, expectedKernelLinkIDs[i], link.KernelLinkID,
			"link at position %d has wrong kernel_link_id", i)
	}
}

// TestListTCXLinksByInterface_FiltersByInterfaceAndDirection verifies that
// only links matching the specified nsid, ifindex, and direction are returned.
func TestListTCXLinksByInterface_FiltersByInterfaceAndDirection(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program for the links to reference.
	progID := kernel.ProgramID(100)
	prog := testProgram()
	prog.Load = bpfman.TestLoadSpec(bpfman.ProgramTypeTCX)
	require.NoError(t, store.Save(ctx, progID, prog), "Save program failed")

	const nsid = uint64(4026531840)

	// Create links on different interfaces and directions.
	testLinks := []struct {
		linkID    uint32
		ifindex   uint32
		direction bpfman.TCDirection
		priority  int32
	}{
		{linkID: 1001, ifindex: 2, direction: bpfman.TCDirectionIngress, priority: 100},
		{linkID: 1002, ifindex: 2, direction: bpfman.TCDirectionIngress, priority: 200},
		{linkID: 1003, ifindex: 2, direction: bpfman.TCDirectionEgress, priority: 100},  // different direction
		{linkID: 1004, ifindex: 3, direction: bpfman.TCDirectionIngress, priority: 100}, // different interface
	}

	for _, link := range testLinks {
		details := bpfman.TCXDetails{
			Interface: "eth0",
			Ifindex:   link.ifindex,
			Direction: link.direction,
			Priority:  link.priority,
			Nsid:      nsid,
		}
		linkID := kernel.LinkID(link.linkID)
		spec := bpfman.NewEphemeralLinkRecord(linkID, progID, details, time.Now())
		err := store.SaveLink(ctx, spec)
		require.NoError(t, err, "SaveLink failed for link %d", link.linkID)
	}

	// Query for ifindex=2, ingress - should return only 2 links.
	links, err := store.ListTCXLinksByInterface(ctx, nsid, 2, "ingress")
	require.NoError(t, err)
	require.Len(t, links, 2, "expected 2 links for ifindex=2, ingress")
	assert.Equal(t, kernel.LinkID(1001), links[0].KernelLinkID)
	assert.Equal(t, kernel.LinkID(1002), links[1].KernelLinkID)

	// Query for ifindex=2, egress - should return only 1 link.
	links, err = store.ListTCXLinksByInterface(ctx, nsid, 2, "egress")
	require.NoError(t, err)
	require.Len(t, links, 1, "expected 1 link for ifindex=2, egress")
	assert.Equal(t, kernel.LinkID(1003), links[0].KernelLinkID)

	// Query for ifindex=3, ingress - should return only 1 link.
	links, err = store.ListTCXLinksByInterface(ctx, nsid, 3, "ingress")
	require.NoError(t, err)
	require.Len(t, links, 1, "expected 1 link for ifindex=3, ingress")
	assert.Equal(t, kernel.LinkID(1004), links[0].KernelLinkID)

	// Query for non-existent interface - should return empty.
	links, err = store.ListTCXLinksByInterface(ctx, nsid, 99, "ingress")
	require.NoError(t, err)
	require.Len(t, links, 0, "expected 0 links for non-existent interface")
}

// TestListTCXLinksByInterface_EmptyResult verifies that querying for
// an interface with no TCX links returns an empty slice, not nil.
func TestListTCXLinksByInterface_EmptyResult(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	links, err := store.ListTCXLinksByInterface(ctx, 4026531840, 2, "ingress")
	require.NoError(t, err, "ListTCXLinksByInterface should not error for empty result")
	assert.NotNil(t, links, "result should not be nil")
	assert.Empty(t, links, "result should be empty")
}

// -----------------------------------------------------------------------------
// GC Tests
// -----------------------------------------------------------------------------

func TestGC_EmptyStore(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// GC with empty kernel state on empty store
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{}, map[kernel.LinkID]bool{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ProgramsRemoved)
	assert.Equal(t, 0, result.DispatchersRemoved)
	assert.Equal(t, 0, result.LinksRemoved)
}

func TestGC_AllProgramsInKernel(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Save a program
	prog := testProgram()
	err = store.Save(ctx, kernel.ProgramID(100), prog)
	require.NoError(t, err)

	// GC with program ID in kernel - nothing should be removed
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{100: true}, map[kernel.LinkID]bool{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ProgramsRemoved)

	// Verify program still exists
	_, err = store.Get(ctx, kernel.ProgramID(100))
	require.NoError(t, err, "program should still exist")
}

func TestGC_StalePrograms(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Save multiple programs
	prog := testProgram()
	err = store.Save(ctx, kernel.ProgramID(100), prog)
	require.NoError(t, err)
	err = store.Save(ctx, kernel.ProgramID(101), prog)
	require.NoError(t, err)
	err = store.Save(ctx, kernel.ProgramID(102), prog)
	require.NoError(t, err)

	// GC with only program 100 in kernel - 101 and 102 should be removed
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{100: true}, map[kernel.LinkID]bool{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.ProgramsRemoved)

	// Verify 100 still exists, 101 and 102 are gone
	_, err = store.Get(ctx, kernel.ProgramID(100))
	require.NoError(t, err, "program 100 should still exist")
	_, err = store.Get(ctx, kernel.ProgramID(101))
	require.Error(t, err, "program 101 should be deleted")
	_, err = store.Get(ctx, kernel.ProgramID(102))
	require.Error(t, err, "program 102 should be deleted")
}

func TestGC_MapOwnerOrdering(t *testing.T) {
	// Test that GC correctly handles map_owner_id FK constraint.
	// Programs with MapOwnerID (dependents) must be deleted before
	// the programs they reference (owners).
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create owner first (must exist for FK)
	owner := testProgram()
	owner.Meta.Name = "owner"
	err = store.Save(ctx, kernel.ProgramID(100), owner)
	require.NoError(t, err)

	// Create dependents that reference the owner
	ownerID := kernel.ProgramID(100)
	dep1 := testProgram()
	dep1.Meta.Name = "dep1"
	dep1.Handles.MapOwnerID = &ownerID
	err = store.Save(ctx, kernel.ProgramID(101), dep1)
	require.NoError(t, err)

	dep2 := testProgram()
	dep2.Meta.Name = "dep2"
	dep2.Handles.MapOwnerID = &ownerID
	err = store.Save(ctx, kernel.ProgramID(102), dep2)
	require.NoError(t, err)

	// GC with empty kernel state - all should be removed
	// If ordering is wrong, FK constraint will fail
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{}, map[kernel.LinkID]bool{})
	require.NoError(t, err, "GC should handle FK ordering correctly")
	assert.Equal(t, 3, result.ProgramsRemoved)

	// Verify all are gone
	_, err = store.Get(ctx, kernel.ProgramID(100))
	require.Error(t, err)
	_, err = store.Get(ctx, kernel.ProgramID(101))
	require.Error(t, err)
	_, err = store.Get(ctx, kernel.ProgramID(102))
	require.Error(t, err)
}

func TestGC_StaleDispatchers(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create dispatchers referencing different program IDs
	disp1 := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  2,
		Revision: 1,
		KernelID: 100,
		LinkID:   200,
	}
	err = store.SaveDispatcher(ctx, disp1)
	require.NoError(t, err)

	// TC dispatchers have LinkID=0 (they use netlink filters, not BPF links)
	disp2 := dispatcher.State{
		Type:     dispatcher.DispatcherTypeTCIngress,
		Nsid:     4026531840,
		Ifindex:  3,
		Revision: 1,
		KernelID: 101,
		LinkID:   0,
	}
	err = store.SaveDispatcher(ctx, disp2)
	require.NoError(t, err)

	// GC with only program 100 in kernel - dispatcher for 101 should be removed
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{100: true}, map[kernel.LinkID]bool{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.DispatchersRemoved)

	// Verify disp1 still exists, disp2 is gone
	_, err = store.GetDispatcher(ctx, "xdp", 4026531840, 2)
	require.NoError(t, err, "dispatcher for program 100 should exist")
	_, err = store.GetDispatcher(ctx, "tc-ingress", 4026531840, 3)
	require.Error(t, err, "dispatcher for program 101 should be deleted")
}

func TestGC_StaleLinks(t *testing.T) {
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create a program first (FK requirement)
	prog := testProgram()
	err = store.Save(ctx, kernel.ProgramID(100), prog)
	require.NoError(t, err)

	// Create links
	details1 := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_enter_openat"}
	linkID1 := kernel.LinkID(200)
	spec1 := bpfman.NewEphemeralLinkRecord(linkID1, kernel.ProgramID(100), details1, time.Now())
	err = store.SaveLink(ctx, spec1)
	require.NoError(t, err)

	details2 := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_exit_openat"}
	linkID2 := kernel.LinkID(201)
	spec2 := bpfman.NewEphemeralLinkRecord(linkID2, kernel.ProgramID(100), details2, time.Now())
	err = store.SaveLink(ctx, spec2)
	require.NoError(t, err)

	// GC with program in kernel but only link 200 in kernel
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{100: true}, map[kernel.LinkID]bool{200: true})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ProgramsRemoved)
	assert.Equal(t, 1, result.LinksRemoved)

	// Verify link 200 exists, link 201 is gone
	links, err := store.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, kernel.LinkID(200), links[0].ID)
}

func TestGC_Comprehensive(t *testing.T) {
	// Test GC with mixed stale entries across all types
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create programs: 100 (alive), 101 (stale owner), 102 (stale dependent)
	prog := testProgram()
	err = store.Save(ctx, kernel.ProgramID(100), prog)
	require.NoError(t, err)

	ownerProg := testProgram()
	ownerProg.Meta.Name = "stale_owner"
	err = store.Save(ctx, kernel.ProgramID(101), ownerProg)
	require.NoError(t, err)

	staleOwnerID := kernel.ProgramID(101)
	depProg := testProgram()
	depProg.Meta.Name = "stale_dep"
	depProg.Handles.MapOwnerID = &staleOwnerID
	err = store.Save(ctx, kernel.ProgramID(102), depProg)
	require.NoError(t, err)

	// Create dispatchers: one for alive program, one for stale
	aliveDisp := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  2,
		Revision: 1,
		KernelID: 100,
		LinkID:   300,
	}
	err = store.SaveDispatcher(ctx, aliveDisp)
	require.NoError(t, err)

	// TC dispatchers have LinkID=0 (they use netlink filters, not BPF links)
	staleDisp := dispatcher.State{
		Type:     dispatcher.DispatcherTypeTCIngress,
		Nsid:     4026531840,
		Ifindex:  3,
		Revision: 1,
		KernelID: 101,
		LinkID:   0,
	}
	err = store.SaveDispatcher(ctx, staleDisp)
	require.NoError(t, err)

	// Create links: one alive, one stale
	aliveDetails := bpfman.TracepointDetails{Group: "syscalls", Name: "test"}
	aliveLinkID := kernel.LinkID(400)
	aliveSpec := bpfman.NewEphemeralLinkRecord(aliveLinkID, kernel.ProgramID(100), aliveDetails, time.Now())
	err = store.SaveLink(ctx, aliveSpec)
	require.NoError(t, err)

	staleDetails := bpfman.TracepointDetails{Group: "syscalls", Name: "test2"}
	staleLinkID := kernel.LinkID(401)
	staleSpec := bpfman.NewEphemeralLinkRecord(staleLinkID, kernel.ProgramID(100), staleDetails, time.Now())
	err = store.SaveLink(ctx, staleSpec)
	require.NoError(t, err)

	// GC with only program 100 and link 400 in kernel
	result, err := store.GC(ctx,
		map[kernel.ProgramID]bool{100: true},
		map[kernel.LinkID]bool{400: true})
	require.NoError(t, err)

	// Should remove: 2 programs (101, 102), 2 dispatchers (101 stale program,
	// 100 has no extension links), 1 link
	assert.Equal(t, 2, result.ProgramsRemoved, "should remove 2 stale programs")
	assert.Equal(t, 2, result.DispatchersRemoved, "should remove 2 dispatchers")
	assert.Equal(t, 1, result.LinksRemoved, "should remove 1 stale link")

	// Verify remaining state
	programs, err := store.List(ctx)
	require.NoError(t, err)
	assert.Len(t, programs, 1, "should have 1 program remaining")
	_, exists := programs[100]
	assert.True(t, exists, "program 100 should exist")

	dispatchers, err := store.ListDispatchers(ctx)
	require.NoError(t, err)
	assert.Len(t, dispatchers, 0, "should have 0 dispatchers remaining")

	links, err := store.ListLinks(ctx)
	require.NoError(t, err)
	assert.Len(t, links, 1, "should have 1 link remaining")
	assert.Equal(t, kernel.LinkID(400), links[0].ID)
}

func TestListLinks_ReturnsDetails(t *testing.T) {
	// Verify that ListLinks() returns LinkSpec with Details populated
	// for ALL link detail types. This is critical for inspect.Snapshot()
	// to build a complete World where the ATTACH column can display
	// meaningful information.
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create a program first (FK requirement for links)
	prog := testProgram()
	require.NoError(t, store.Save(ctx, kernel.ProgramID(100), prog), "Save program failed")

	// Create dispatchers for XDP and TC links (FK requirement for their details)
	xdpDispatcher := dispatcher.State{
		Type:     dispatcher.DispatcherTypeXDP,
		Nsid:     4026531840,
		Ifindex:  2,
		Revision: 1,
		KernelID: 500,
		LinkID:   501,
	}
	require.NoError(t, store.SaveDispatcher(ctx, xdpDispatcher), "SaveDispatcher XDP failed")

	tcDispatcher := dispatcher.State{
		Type:     dispatcher.DispatcherTypeTCIngress,
		Nsid:     4026531840,
		Ifindex:  3,
		Revision: 1,
		KernelID: 502,
		LinkID:   0, // TC dispatchers don't have links
	}
	require.NoError(t, store.SaveDispatcher(ctx, tcDispatcher), "SaveDispatcher TC failed")

	// Create links with ALL detail types
	testCases := []struct {
		linkID  kernel.LinkID
		details bpfman.LinkDetails
		check   func(t *testing.T, got bpfman.LinkDetails)
	}{
		{
			linkID:  10,
			details: bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.TracepointDetails)
				require.True(t, ok, "expected TracepointDetails, got %T", got)
				assert.Equal(t, "sched", d.Group)
				assert.Equal(t, "sched_switch", d.Name)
			},
		},
		{
			linkID:  20,
			details: bpfman.KprobeDetails{FnName: "do_sys_open", Offset: 64, Retprobe: true},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.KprobeDetails)
				require.True(t, ok, "expected KprobeDetails, got %T", got)
				assert.Equal(t, "do_sys_open", d.FnName)
				assert.Equal(t, uint64(64), d.Offset)
				assert.True(t, d.Retprobe)
			},
		},
		{
			linkID:  30,
			details: bpfman.UprobeDetails{Target: "/usr/bin/test", FnName: "main", Offset: 128, PID: 1234, Retprobe: false},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.UprobeDetails)
				require.True(t, ok, "expected UprobeDetails, got %T", got)
				assert.Equal(t, "/usr/bin/test", d.Target)
				assert.Equal(t, "main", d.FnName)
				assert.Equal(t, uint64(128), d.Offset)
				assert.Equal(t, int32(1234), d.PID)
				assert.False(t, d.Retprobe)
			},
		},
		{
			linkID:  40,
			details: bpfman.FentryDetails{FnName: "tcp_connect"},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.FentryDetails)
				require.True(t, ok, "expected FentryDetails, got %T", got)
				assert.Equal(t, "tcp_connect", d.FnName)
			},
		},
		{
			linkID:  50,
			details: bpfman.FexitDetails{FnName: "tcp_disconnect"},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.FexitDetails)
				require.True(t, ok, "expected FexitDetails, got %T", got)
				assert.Equal(t, "tcp_disconnect", d.FnName)
			},
		},
		{
			linkID: 60,
			details: bpfman.XDPDetails{
				Interface:    "eth0",
				Ifindex:      2,
				Priority:     50,
				Position:     1,
				ProceedOn:    []int32{2, 31}, // XDP_PASS=2, XDP_DISPATCHER_RETURN=31
				Netns:        "/proc/1/ns/net",
				Nsid:         4026531840,
				DispatcherID: 500, // References XDP dispatcher created above
				Revision:     1,
			},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.XDPDetails)
				require.True(t, ok, "expected XDPDetails, got %T", got)
				assert.Equal(t, "eth0", d.Interface)
				assert.Equal(t, uint32(2), d.Ifindex)
				assert.Equal(t, int32(50), d.Priority)
				assert.Equal(t, int32(1), d.Position)
				assert.Equal(t, []int32{2, 31}, d.ProceedOn)
				assert.Equal(t, "/proc/1/ns/net", d.Netns)
				assert.Equal(t, uint64(4026531840), d.Nsid)
				assert.Equal(t, kernel.ProgramID(500), d.DispatcherID)
				assert.Equal(t, uint32(1), d.Revision)
			},
		},
		{
			linkID: 70,
			details: bpfman.TCDetails{
				Interface:    "eth1",
				Ifindex:      3,
				Direction:    "ingress",
				Priority:     100,
				Position:     1,
				ProceedOn:    []int32{0, 3}, // TC_ACT_OK=0, TC_ACT_PIPE=3
				Netns:        "/proc/1/ns/net",
				Nsid:         4026531840,
				DispatcherID: 502, // References TC dispatcher created above
				Revision:     1,
			},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.TCDetails)
				require.True(t, ok, "expected TCDetails, got %T", got)
				assert.Equal(t, "eth1", d.Interface)
				assert.Equal(t, uint32(3), d.Ifindex)
				assert.Equal(t, bpfman.TCDirection("ingress"), d.Direction)
				assert.Equal(t, int32(100), d.Priority)
				assert.Equal(t, int32(1), d.Position)
				assert.Equal(t, []int32{0, 3}, d.ProceedOn)
				assert.Equal(t, "/proc/1/ns/net", d.Netns)
				assert.Equal(t, uint64(4026531840), d.Nsid)
			},
		},
		{
			linkID: 80,
			details: bpfman.TCXDetails{
				Interface: "eth2",
				Ifindex:   4,
				Direction: "egress",
				Priority:  200,
				Netns:     "/proc/1/ns/net",
				Nsid:      4026531840,
			},
			check: func(t *testing.T, got bpfman.LinkDetails) {
				d, ok := got.(bpfman.TCXDetails)
				require.True(t, ok, "expected TCXDetails, got %T", got)
				assert.Equal(t, "eth2", d.Interface)
				assert.Equal(t, uint32(4), d.Ifindex)
				assert.Equal(t, bpfman.TCDirection("egress"), d.Direction)
				assert.Equal(t, int32(200), d.Priority)
				assert.Equal(t, "/proc/1/ns/net", d.Netns)
				assert.Equal(t, uint64(4026531840), d.Nsid)
			},
		},
	}

	// Save all links
	for _, tc := range testCases {
		spec := bpfman.NewEphemeralLinkRecord(tc.linkID, kernel.ProgramID(100), tc.details, time.Now())
		require.NoError(t, store.SaveLink(ctx, spec), "SaveLink %d failed", tc.linkID)
	}

	// ListLinks should return links WITH details populated
	links, err := store.ListLinks(ctx)
	require.NoError(t, err, "ListLinks failed")
	require.Len(t, links, len(testCases), "expected %d links", len(testCases))

	// Build a map for easier lookup
	linksByID := make(map[kernel.LinkID]bpfman.LinkRecord)
	for _, l := range links {
		linksByID[l.ID] = l
	}

	// Verify each link's details
	for _, tc := range testCases {
		t.Run(string(tc.details.Kind()), func(t *testing.T) {
			link, ok := linksByID[tc.linkID]
			require.True(t, ok, "link %d not found", tc.linkID)
			require.NotNil(t, link.Details, "link %d Details should not be nil", tc.linkID)
			tc.check(t, link.Details)
		})
	}
}

func TestListLinksByProgram_ReturnsDetails(t *testing.T) {
	// Verify that ListLinksByProgram() also returns details.
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	defer store.Close()

	ctx := context.Background()

	// Create two programs
	prog := testProgram()
	require.NoError(t, store.Save(ctx, kernel.ProgramID(100), prog), "Save program 100 failed")
	require.NoError(t, store.Save(ctx, kernel.ProgramID(200), prog), "Save program 200 failed")

	// Create links for program 100
	tp1 := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_enter_read"}
	require.NoError(t, store.SaveLink(ctx, bpfman.NewEphemeralLinkRecord(kernel.LinkID(10), kernel.ProgramID(100), tp1, time.Now())))

	tp2 := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_exit_read"}
	require.NoError(t, store.SaveLink(ctx, bpfman.NewEphemeralLinkRecord(kernel.LinkID(11), kernel.ProgramID(100), tp2, time.Now())))

	// Create link for program 200
	tp3 := bpfman.TracepointDetails{Group: "syscalls", Name: "sys_enter_write"}
	require.NoError(t, store.SaveLink(ctx, bpfman.NewEphemeralLinkRecord(kernel.LinkID(20), kernel.ProgramID(200), tp3, time.Now())))

	// ListLinksByProgram for program 100 should return 2 links with details
	links, err := store.ListLinksByProgram(ctx, kernel.ProgramID(100))
	require.NoError(t, err)
	require.Len(t, links, 2)

	for _, link := range links {
		require.NotNil(t, link.Details, "link %d Details should not be nil", link.ID)
		_, ok := link.Details.(bpfman.TracepointDetails)
		require.True(t, ok, "expected TracepointDetails for link %d", link.ID)
	}
}

func TestGC_SyntheticLinkIDsSkipped(t *testing.T) {
	// Test that GC skips links with nil kernel_link_id.
	// These are used for perf_event-based links (e.g., container uprobes)
	// that cannot be enumerated via the kernel's link iterator.
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create a program first (FK requirement)
	prog := testProgram()
	err = store.Save(ctx, kernel.ProgramID(100), prog)
	require.NoError(t, err)

	// Create a real kernel link (kernel_link_id = 200)
	realDetails := bpfman.UprobeDetails{Target: "/usr/bin/test", FnName: "main"}
	realLinkID := kernel.LinkID(200)
	realSpec := bpfman.NewEphemeralLinkRecord(realLinkID, kernel.ProgramID(100), realDetails, time.Now())
	err = store.SaveLink(ctx, realSpec)
	require.NoError(t, err)

	// Create a synthetic link (high-range ID = synthetic)
	// This simulates a container uprobe with perf_event-based link
	syntheticDetails := bpfman.UprobeDetails{Target: "/app/binary", FnName: "handler", ContainerPid: 12345}
	syntheticLinkID := kernel.LinkID(0x80000001) // synthetic range
	syntheticSpec := bpfman.NewEphemeralLinkRecord(syntheticLinkID, kernel.ProgramID(100), syntheticDetails, time.Now())
	err = store.SaveLink(ctx, syntheticSpec)
	require.NoError(t, err)

	// Verify both links exist
	links, err := store.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, links, 2)

	// GC with program in kernel but only real link 200 in kernel
	// (synthetic link cannot be in kernelLinkIDs since it has no kernel ID)
	result, err := store.GC(ctx, map[kernel.ProgramID]bool{100: true}, map[kernel.LinkID]bool{200: true})
	require.NoError(t, err)

	// Should NOT remove synthetic link even though it's not in kernelLinkIDs
	assert.Equal(t, 0, result.ProgramsRemoved, "should not remove any programs")
	assert.Equal(t, 0, result.LinksRemoved, "should not remove any links (synthetic should be skipped)")

	// Verify both links still exist
	links, err = store.ListLinks(ctx)
	require.NoError(t, err)
	assert.Len(t, links, 2, "both links should remain")

	// Find both links by ID and verify synthetic detection
	var foundReal, foundSynthetic bool
	for _, link := range links {
		if link.ID == realLinkID {
			assert.False(t, link.IsSynthetic(), "real link should not be synthetic")
			foundReal = true
		}
		if link.ID == syntheticLinkID {
			assert.True(t, link.IsSynthetic(), "synthetic link should be synthetic")
			foundSynthetic = true
		}
	}
	assert.True(t, foundReal, "real link should exist")
	assert.True(t, foundSynthetic, "synthetic link should exist")
}
