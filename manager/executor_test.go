package manager_test

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

// stubStore is a minimal Store implementation for executor tests.
// Only implements methods used by SaveProgram/DeleteProgram actions.
// All other methods panic - if a test hits them, it's using the wrong action type.
type stubStore struct {
	saveFunc   func(ctx context.Context, kernelID uint32) error
	deleteFunc func(ctx context.Context, kernelID uint32) error
}

func newStubStore() *stubStore {
	return &stubStore{
		saveFunc:   func(ctx context.Context, kernelID uint32) error { return nil },
		deleteFunc: func(ctx context.Context, kernelID uint32) error { return nil },
	}
}

// ProgramWriter methods (used by tests)
func (s *stubStore) Save(ctx context.Context, kernelID uint32, _ bpfman.ProgramRecord) error {
	return s.saveFunc(ctx, kernelID)
}

func (s *stubStore) Delete(ctx context.Context, kernelID uint32) error {
	return s.deleteFunc(ctx, kernelID)
}

// io.Closer
func (s *stubStore) Close() error { return nil }

// ProgramReader
func (s *stubStore) Get(ctx context.Context, kernelID uint32) (bpfman.ProgramRecord, error) {
	panic("stubStore.Get not implemented")
}

// ProgramLister
func (s *stubStore) List(ctx context.Context) (map[uint32]bpfman.ProgramRecord, error) {
	panic("stubStore.List not implemented")
}

// ProgramFinder
func (s *stubStore) FindProgramByMetadata(ctx context.Context, key, value string) (bpfman.ProgramRecord, uint32, error) {
	panic("stubStore.FindProgramByMetadata not implemented")
}

// MapOwnershipReader
func (s *stubStore) CountDependentPrograms(ctx context.Context, kernelID uint32) (int, error) {
	panic("stubStore.CountDependentPrograms not implemented")
}

// LinkWriter
func (s *stubStore) SaveLink(ctx context.Context, record bpfman.LinkRecord) error {
	panic("stubStore.SaveLink not implemented")
}

func (s *stubStore) DeleteLink(ctx context.Context, linkID bpfman.LinkID) error {
	panic("stubStore.DeleteLink not implemented")
}

// LinkReader
func (s *stubStore) GetLink(ctx context.Context, linkID bpfman.LinkID) (bpfman.LinkRecord, error) {
	panic("stubStore.GetLink not implemented")
}

// LinkLister
func (s *stubStore) ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error) {
	panic("stubStore.ListLinks not implemented")
}

func (s *stubStore) ListLinksByProgram(ctx context.Context, programKernelID uint32) ([]bpfman.LinkRecord, error) {
	panic("stubStore.ListLinksByProgram not implemented")
}

func (s *stubStore) ListTCXLinksByInterface(ctx context.Context, nsid uint64, ifindex uint32, direction string) ([]bpfman.TCXLinkInfo, error) {
	panic("stubStore.ListTCXLinksByInterface not implemented")
}

// DispatcherStore
func (s *stubStore) GetDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (dispatcher.State, error) {
	panic("stubStore.GetDispatcher not implemented")
}

func (s *stubStore) ListDispatchers(ctx context.Context) ([]dispatcher.State, error) {
	panic("stubStore.ListDispatchers not implemented")
}

func (s *stubStore) SaveDispatcher(ctx context.Context, state dispatcher.State) error {
	panic("stubStore.SaveDispatcher not implemented")
}

func (s *stubStore) DeleteDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) error {
	panic("stubStore.DeleteDispatcher not implemented")
}

func (s *stubStore) IncrementRevision(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (uint32, error) {
	panic("stubStore.IncrementRevision not implemented")
}

func (s *stubStore) CountDispatcherLinks(ctx context.Context, dispatcherKernelID uint32) (int, error) {
	panic("stubStore.CountDispatcherLinks not implemented")
}

// Transactional
func (s *stubStore) RunInTransaction(ctx context.Context, fn func(platform.Store) error) error {
	panic("stubStore.RunInTransaction not implemented")
}

// GarbageCollector
func (s *stubStore) GC(ctx context.Context, kernelProgramIDs, kernelLinkIDs map[uint32]bool) (platform.GCResult, error) {
	panic("stubStore.GC not implemented")
}

// stubKernel is a minimal KernelOperations implementation for executor tests.
// All methods panic - we only use store actions in these tests.
type stubKernel struct{}

func newStubKernel() *stubKernel { return &stubKernel{} }

// KernelSource
func (k *stubKernel) Programs(ctx context.Context) iter.Seq2[kernel.Program, error] {
	panic("stubKernel.Programs not implemented")
}

func (k *stubKernel) GetProgramByID(ctx context.Context, id uint32) (kernel.Program, error) {
	panic("stubKernel.GetProgramByID not implemented")
}

func (k *stubKernel) GetProgramStatsByID(ctx context.Context, id uint32) (*kernel.ProgramStats, error) {
	panic("stubKernel.GetProgramStatsByID not implemented")
}

func (k *stubKernel) GetLinkByID(ctx context.Context, id uint32) (kernel.Link, error) {
	panic("stubKernel.GetLinkByID not implemented")
}

func (k *stubKernel) GetMapByID(ctx context.Context, id uint32) (kernel.Map, error) {
	panic("stubKernel.GetMapByID not implemented")
}

func (k *stubKernel) Maps(ctx context.Context) iter.Seq2[kernel.Map, error] {
	panic("stubKernel.Maps not implemented")
}

func (k *stubKernel) Links(ctx context.Context) iter.Seq2[kernel.Link, error] {
	panic("stubKernel.Links not implemented")
}

// ProgramLoader
func (k *stubKernel) Load(ctx context.Context, spec bpfman.LoadSpec, _ bpfmanfs.BPFFS) (bpfman.LoadOutput, error) {
	panic("stubKernel.Load not implemented")
}

// ProgramUnloader
func (k *stubKernel) Unload(ctx context.Context, pinPath string) error {
	panic("stubKernel.Unload not implemented")
}

func (k *stubKernel) UnloadProgram(ctx context.Context, progPinPath, mapsDir string) error {
	panic("stubKernel.UnloadProgram not implemented")
}

// PinInspector
func (k *stubKernel) ListPinDir(ctx context.Context, pinDir string, includeMaps bool) (*kernel.PinDirContents, error) {
	panic("stubKernel.ListPinDir not implemented")
}

func (k *stubKernel) GetPinned(ctx context.Context, pinPath string) (*kernel.PinnedProgram, error) {
	panic("stubKernel.GetPinned not implemented")
}

// ProgramAttacher
func (k *stubKernel) AttachTracepoint(ctx context.Context, progPinPath, group, name, linkPinPath string) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachTracepoint not implemented")
}

func (k *stubKernel) AttachXDP(ctx context.Context, progPinPath string, ifindex int, linkPinPath string) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachXDP not implemented")
}

func (k *stubKernel) AttachKprobe(ctx context.Context, progPinPath, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachKprobe not implemented")
}

func (k *stubKernel) AttachUprobeLocal(ctx context.Context, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachUprobeLocal not implemented")
}

func (k *stubKernel) AttachUprobeContainer(ctx context.Context, scope lock.WriterScope, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string, containerPid int32) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachUprobeContainer not implemented")
}

func (k *stubKernel) AttachFentry(ctx context.Context, progPinPath, fnName, linkPinPath string) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachFentry not implemented")
}

func (k *stubKernel) AttachFexit(ctx context.Context, progPinPath, fnName, linkPinPath string) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachFexit not implemented")
}

// DispatcherAttacher
func (k *stubKernel) AttachXDPDispatcher(ctx context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*platform.XDPDispatcherResult, error) {
	panic("stubKernel.AttachXDPDispatcher not implemented")
}

func (k *stubKernel) AttachXDPExtension(ctx context.Context, spec dispatcher.XDPExtensionAttachSpec) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachXDPExtension not implemented")
}

func (k *stubKernel) AttachTCDispatcher(ctx context.Context, spec dispatcher.TCDispatcherAttachSpec) (*platform.TCDispatcherResult, error) {
	panic("stubKernel.AttachTCDispatcher not implemented")
}

func (k *stubKernel) AttachTCExtension(ctx context.Context, spec dispatcher.TCExtensionAttachSpec) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachTCExtension not implemented")
}

func (k *stubKernel) AttachTCX(ctx context.Context, ifindex int, direction, programPinPath, linkPinPath, netns string, order bpfman.TCXAttachOrder) (bpfman.AttachOutput, error) {
	panic("stubKernel.AttachTCX not implemented")
}

// LinkDetacher
func (k *stubKernel) DetachLink(ctx context.Context, linkPinPath string) error {
	panic("stubKernel.DetachLink not implemented")
}

// PinRemover
func (k *stubKernel) RemovePin(ctx context.Context, path string) error {
	panic("stubKernel.RemovePin not implemented")
}

// MapRepinner
func (k *stubKernel) RepinMap(ctx context.Context, srcPath, dstPath string) error {
	panic("stubKernel.RepinMap not implemented")
}

// TCFilterDetacher
func (k *stubKernel) DetachTCFilter(ctx context.Context, ifindex int, ifname string, parent uint32, priority uint16, handle uint32) error {
	panic("stubKernel.DetachTCFilter not implemented")
}

func (k *stubKernel) FindTCFilterHandle(ctx context.Context, ifindex int, parent uint32, priority uint16) (uint32, error) {
	panic("stubKernel.FindTCFilterHandle not implemented")
}

// The remaining methods panic if called - tests should only use store actions.
// This keeps the test focused on executor counting logic, not kernel behaviour.

func TestExecuteAllWithResult_AllSucceed(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()
	exec := manager.NewExecutorForTest(store, kernel)

	execWithResult, ok := exec.(manager.ActionExecutorWithResult)
	if !ok {
		t.Fatal("executor does not implement ActionExecutorWithResult")
	}

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
		manager.SaveProgram{KernelID: 3},
	}

	result := execWithResult.ExecuteAllWithResult(context.Background(), actions)

	// Invariant: on success, FailedIndex == -1
	if result.FailedIndex != -1 {
		t.Errorf("FailedIndex = %d, want -1", result.FailedIndex)
	}
	// Invariant: on success, Error == nil
	if result.Error != nil {
		t.Errorf("Error = %v, want nil", result.Error)
	}
	// Invariant: on success, CompletedCount == len(actions)
	if result.CompletedCount != len(actions) {
		t.Errorf("CompletedCount = %d, want %d", result.CompletedCount, len(actions))
	}
	// Actions slice is preserved
	if len(result.Actions) != len(actions) {
		t.Errorf("Actions length = %d, want %d", len(result.Actions), len(actions))
	}
}

func TestExecuteAllWithResult_EmptySlice(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()
	exec := manager.NewExecutorForTest(store, kernel)
	execWithResult := exec.(manager.ActionExecutorWithResult)

	result := execWithResult.ExecuteAllWithResult(context.Background(), nil)

	if result.FailedIndex != -1 {
		t.Errorf("FailedIndex = %d, want -1", result.FailedIndex)
	}
	if result.Error != nil {
		t.Errorf("Error = %v, want nil", result.Error)
	}
	if result.CompletedCount != 0 {
		t.Errorf("CompletedCount = %d, want 0", result.CompletedCount)
	}
}

func TestExecuteAllWithResult_FirstActionFails(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()

	expectedErr := errors.New("first action failed")
	store.saveFunc = func(ctx context.Context, kernelID uint32) error {
		return expectedErr
	}

	exec := manager.NewExecutorForTest(store, kernel)
	execWithResult := exec.(manager.ActionExecutorWithResult)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
		manager.SaveProgram{KernelID: 3},
	}

	result := execWithResult.ExecuteAllWithResult(context.Background(), actions)

	// Invariant: on failure, FailedIndex == CompletedCount
	if result.FailedIndex != result.CompletedCount {
		t.Errorf("FailedIndex (%d) != CompletedCount (%d)", result.FailedIndex, result.CompletedCount)
	}
	if result.FailedIndex != 0 {
		t.Errorf("FailedIndex = %d, want 0", result.FailedIndex)
	}
	if result.CompletedCount != 0 {
		t.Errorf("CompletedCount = %d, want 0", result.CompletedCount)
	}
	if !errors.Is(result.Error, expectedErr) {
		t.Errorf("Error = %v, want %v", result.Error, expectedErr)
	}
}

func TestExecuteAllWithResult_MiddleActionFails(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()

	expectedErr := errors.New("middle action failed")
	store.saveFunc = func(ctx context.Context, kernelID uint32) error {
		if kernelID == 2 {
			return expectedErr
		}
		return nil
	}

	exec := manager.NewExecutorForTest(store, kernel)
	execWithResult := exec.(manager.ActionExecutorWithResult)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
		manager.SaveProgram{KernelID: 3},
	}

	result := execWithResult.ExecuteAllWithResult(context.Background(), actions)

	// Invariant: on failure, FailedIndex == CompletedCount
	if result.FailedIndex != result.CompletedCount {
		t.Errorf("FailedIndex (%d) != CompletedCount (%d)", result.FailedIndex, result.CompletedCount)
	}
	if result.FailedIndex != 1 {
		t.Errorf("FailedIndex = %d, want 1", result.FailedIndex)
	}
	if result.CompletedCount != 1 {
		t.Errorf("CompletedCount = %d, want 1", result.CompletedCount)
	}
	if !errors.Is(result.Error, expectedErr) {
		t.Errorf("Error = %v, want %v", result.Error, expectedErr)
	}
}

func TestExecuteAllWithResult_LastActionFails(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()

	expectedErr := errors.New("last action failed")
	store.saveFunc = func(ctx context.Context, kernelID uint32) error {
		if kernelID == 3 {
			return expectedErr
		}
		return nil
	}

	exec := manager.NewExecutorForTest(store, kernel)
	execWithResult := exec.(manager.ActionExecutorWithResult)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
		manager.SaveProgram{KernelID: 3},
	}

	result := execWithResult.ExecuteAllWithResult(context.Background(), actions)

	// Invariant: on failure, FailedIndex == CompletedCount
	if result.FailedIndex != result.CompletedCount {
		t.Errorf("FailedIndex (%d) != CompletedCount (%d)", result.FailedIndex, result.CompletedCount)
	}
	if result.FailedIndex != 2 {
		t.Errorf("FailedIndex = %d, want 2", result.FailedIndex)
	}
	if result.CompletedCount != 2 {
		t.Errorf("CompletedCount = %d, want 2", result.CompletedCount)
	}
	if !errors.Is(result.Error, expectedErr) {
		t.Errorf("Error = %v, want %v", result.Error, expectedErr)
	}
}

func TestExecuteAllWithResult_StopsOnFirstError(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()

	callCount := 0
	store.saveFunc = func(ctx context.Context, kernelID uint32) error {
		callCount++
		if kernelID == 2 {
			return errors.New("stop here")
		}
		return nil
	}

	exec := manager.NewExecutorForTest(store, kernel)
	execWithResult := exec.(manager.ActionExecutorWithResult)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
		manager.SaveProgram{KernelID: 3},
		manager.SaveProgram{KernelID: 4},
	}

	_ = execWithResult.ExecuteAllWithResult(context.Background(), actions)

	// Should have called save exactly twice (1 succeeds, 2 fails, 3 and 4 never called)
	if callCount != 2 {
		t.Errorf("save called %d times, want 2", callCount)
	}
}

func TestExecuteAllWithResult_ActionsSliceUnmodified(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()

	store.saveFunc = func(ctx context.Context, kernelID uint32) error {
		if kernelID == 2 {
			return errors.New("fail")
		}
		return nil
	}

	exec := manager.NewExecutorForTest(store, kernel)
	execWithResult := exec.(manager.ActionExecutorWithResult)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
		manager.SaveProgram{KernelID: 3},
	}

	result := execWithResult.ExecuteAllWithResult(context.Background(), actions)

	// Verify the Actions slice is the original (same pointer)
	if &result.Actions[0] != &actions[0] {
		t.Error("Actions slice is not the original slice")
	}

	// Verify slicing works as expected
	completed := result.Actions[:result.CompletedCount]
	if len(completed) != 1 {
		t.Errorf("completed slice length = %d, want 1", len(completed))
	}

	failed := result.Actions[result.FailedIndex]
	if sp, ok := failed.(manager.SaveProgram); !ok || sp.KernelID != 2 {
		t.Errorf("failed action = %v, want SaveProgram{KernelID: 2}", failed)
	}

	remaining := result.Actions[result.FailedIndex+1:]
	if len(remaining) != 1 {
		t.Errorf("remaining slice length = %d, want 1", len(remaining))
	}
}

func TestExecuteAll_DelegatesToExecuteAllWithResult(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()

	expectedErr := errors.New("expected error")
	store.saveFunc = func(ctx context.Context, kernelID uint32) error {
		if kernelID == 2 {
			return expectedErr
		}
		return nil
	}

	exec := manager.NewExecutorForTest(store, kernel)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
	}

	err := exec.ExecuteAll(context.Background(), actions)

	if !errors.Is(err, expectedErr) {
		t.Errorf("ExecuteAll() = %v, want %v", err, expectedErr)
	}
}

func TestExecuteAll_SuccessReturnsNil(t *testing.T) {
	store := newStubStore()
	kernel := newStubKernel()
	exec := manager.NewExecutorForTest(store, kernel)

	actions := []manager.Action{
		manager.SaveProgram{KernelID: 1},
		manager.SaveProgram{KernelID: 2},
	}

	err := exec.ExecuteAll(context.Background(), actions)

	if err != nil {
		t.Errorf("ExecuteAll() = %v, want nil", err)
	}
}
