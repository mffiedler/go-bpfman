package manager_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// fakeDiscoverer implements interpreter.ProgramDiscoverer for testing.
type fakeDiscoverer struct {
	// Programs maps object path to discovered programs
	programs map[string][]interpreter.DiscoveredProgram
	// DiscoverErr if set, DiscoverPrograms returns this error
	discoverErr error
	// ValidateErr if set, ValidatePrograms returns this error
	validateErr error
}

func newFakeDiscoverer() *fakeDiscoverer {
	return &fakeDiscoverer{
		programs: make(map[string][]interpreter.DiscoveredProgram),
	}
}

// SetPrograms configures the programs to return for a given object path.
func (d *fakeDiscoverer) SetPrograms(objectPath string, programs []interpreter.DiscoveredProgram) {
	d.programs[objectPath] = programs
}

// SetDiscoverError configures DiscoverPrograms to return the given error.
func (d *fakeDiscoverer) SetDiscoverError(err error) {
	d.discoverErr = err
}

// SetValidateError configures ValidatePrograms to return the given error.
func (d *fakeDiscoverer) SetValidateError(err error) {
	d.validateErr = err
}

func (d *fakeDiscoverer) DiscoverPrograms(objectPath string) ([]interpreter.DiscoveredProgram, error) {
	if d.discoverErr != nil {
		return nil, d.discoverErr
	}
	programs, ok := d.programs[objectPath]
	if !ok {
		return nil, fmt.Errorf("no programs found in object file")
	}
	// Return sorted copy for determinism
	result := make([]interpreter.DiscoveredProgram, len(programs))
	copy(result, programs)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (d *fakeDiscoverer) ValidatePrograms(objectPath string, programNames []string) error {
	if d.validateErr != nil {
		return d.validateErr
	}
	programs, ok := d.programs[objectPath]
	if !ok {
		return fmt.Errorf("object file not found: %s", objectPath)
	}
	// Build set of available program names
	available := make(map[string]bool)
	for _, p := range programs {
		available[p.Name] = true
	}
	// Check each requested program
	var missing []string
	for _, name := range programNames {
		if !available[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		availableList := make([]string, 0, len(available))
		for name := range available {
			availableList = append(availableList, name)
		}
		sort.Strings(availableList)
		return fmt.Errorf("program(s) not found: %v; available: %v", missing, availableList)
	}
	return nil
}

// Ensure fakeDiscoverer implements the interface.
var _ interpreter.ProgramDiscoverer = (*fakeDiscoverer)(nil)

// fakeImagePuller implements interpreter.ImagePuller for testing.
type fakeImagePuller struct {
	objectPath string
	digest     string
	pullErr    error
}

func newFakeImagePuller(objectPath string) *fakeImagePuller {
	return &fakeImagePuller{
		objectPath: objectPath,
		digest:     "sha256:fake",
	}
}

func (p *fakeImagePuller) SetPullError(err error) {
	p.pullErr = err
}

func (p *fakeImagePuller) Pull(_ context.Context, ref interpreter.ImageRef) (interpreter.PulledImage, error) {
	if p.pullErr != nil {
		return interpreter.PulledImage{}, p.pullErr
	}
	return interpreter.PulledImage{
		ObjectPath: p.objectPath,
		Digest:     p.digest,
	}, nil
}

// Ensure fakeImagePuller implements the interface.
var _ interpreter.ImagePuller = (*fakeImagePuller)(nil)

// kernelOp records an operation performed on the fake kernel.
type kernelOp struct {
	Op        string // "load", "unload", "attach", "detach", "attach-xdp-ext", "attach-tc-ext"
	Name      string // program or link name
	ID        uint32 // kernel ID assigned
	Err       error  // error if operation failed
	MapPinDir string // for XDP/TC extension attachments, the map directory used
}

// tcFilterKey identifies a TC filter by its location on an interface.
type tcFilterKey struct {
	ifindex  int
	parent   uint32
	priority uint16
}

// fakeKernel implements interpreter.KernelOperations for testing.
// It simulates kernel BPF operations without actual syscalls.
type fakeKernel struct {
	nextID   atomic.Uint32
	programs map[uint32]fakeProgram
	links    map[uint32]*bpfman.Link

	// TC filter handle tracking for FindTCFilterHandle
	tcFilters map[tcFilterKey]uint32

	// Operation recording for verification
	ops        []kernelOp
	removePins []string      // paths passed to RemovePin
	tcDetaches []tcFilterKey // TC filters detached
	mu         sync.Mutex

	// Error injection - set these to control behaviour
	failOnProgram map[string]error // fail Load if program name matches
	failOnNthLoad int              // fail on Nth load (0 = never fail)
	loadCount     int              // track load count for failOnNthLoad

	// Attach error injection
	failOnAttach map[string]error // fail attach by type (e.g., "tracepoint", "kprobe")

	// Detach error injection
	failOnDetach map[uint32]error // fail detach by link ID

	// Interface error injection
	failOnIfname  map[string]error // fail attach if interface name matches
	failOnIfindex map[int]error    // fail attach if interface index matches
}

// fakeProgram stores program data for the fake kernel.
type fakeProgram struct {
	id          uint32
	name        string
	programType bpfman.ProgramType
	pinPath     string
	pinDir      string
}

func newFakeKernel() *fakeKernel {
	fk := &fakeKernel{
		programs:      make(map[uint32]fakeProgram),
		links:         make(map[uint32]*bpfman.Link),
		tcFilters:     make(map[tcFilterKey]uint32),
		failOnProgram: make(map[string]error),
		failOnAttach:  make(map[string]error),
		failOnDetach:  make(map[uint32]error),
		failOnIfname:  make(map[string]error),
		failOnIfindex: make(map[int]error),
	}
	fk.nextID.Store(100)
	return fk
}

// Operations returns a copy of recorded operations for verification.
func (f *fakeKernel) Operations() []kernelOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make([]kernelOp, len(f.ops))
	copy(ops, f.ops)
	return ops
}

// recordOp records an operation for later verification.
func (f *fakeKernel) recordOp(op, name string, id uint32, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, kernelOp{Op: op, Name: name, ID: id, Err: err})
}

// InjectKernelLink adds a link directly to the kernel state without going
// through the normal attach flow. This simulates a link that exists in the
// kernel but is not managed by bpfman.
func (f *fakeKernel) InjectKernelLink(id uint32, kind bpfman.LinkKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links[id] = &bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:   bpfman.LinkID(id),
			Kind: kind,
		},
	}
}

// InjectKernelProgram adds a program directly to the kernel state without going
// through the normal load flow. This simulates a program that exists in the
// kernel but is not managed by bpfman.
func (f *fakeKernel) InjectKernelProgram(id uint32, name string, progType bpfman.ProgramType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.programs[id] = fakeProgram{
		id:          id,
		name:        name,
		programType: progType,
	}
}

// recordExtensionAttach records an XDP/TC extension attachment with the mapPinDir.
func (f *fakeKernel) recordExtensionAttach(op, programName string, id uint32, mapPinDir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, kernelOp{Op: op, Name: programName, ID: id, MapPinDir: mapPinDir})
}

// ExtensionAttachOps returns all XDP/TC extension attach operations.
func (f *fakeKernel) ExtensionAttachOps() []kernelOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ops []kernelOp
	for _, op := range f.ops {
		if op.Op == "attach-xdp-ext" || op.Op == "attach-tc-ext" {
			ops = append(ops, op)
		}
	}
	return ops
}

// recordTCXAttach records a TCX attachment with the programPinPath.
func (f *fakeKernel) recordTCXAttach(programPinPath string, id uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Reuse MapPinDir field to store programPinPath for TCX
	f.ops = append(f.ops, kernelOp{Op: "attach-tcx", Name: programPinPath, ID: id})
}

// TCXAttachOps returns all TCX attach operations.
func (f *fakeKernel) TCXAttachOps() []kernelOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ops []kernelOp
	for _, op := range f.ops {
		if op.Op == "attach-tcx" {
			ops = append(ops, op)
		}
	}
	return ops
}

// FailOnProgram configures the kernel to fail when loading a specific program.
func (f *fakeKernel) FailOnProgram(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnProgram[name] = err
}

// FailOnNthLoad configures the kernel to fail on the Nth load attempt.
func (f *fakeKernel) FailOnNthLoad(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnNthLoad = n
}

// FailOnAttach configures the kernel to fail when attaching a specific type.
// Valid types: "tracepoint", "kprobe", "uprobe", "fentry", "fexit", "xdp", "tc", "tcx"
func (f *fakeKernel) FailOnAttach(attachType string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnAttach[attachType] = err
}

// FailOnDetach configures the kernel to fail when detaching a specific link ID.
func (f *fakeKernel) FailOnDetach(linkID uint32, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnDetach[linkID] = err
}

// FailOnIfname configures the kernel to fail when attaching to a specific interface.
func (f *fakeKernel) FailOnIfname(ifname string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnIfname[ifname] = err
}

// FailOnIfindex configures the kernel to fail when attaching to a specific interface index.
func (f *fakeKernel) FailOnIfindex(ifindex int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnIfindex[ifindex] = err
}

// Reset clears all recorded operations and error injection settings.
func (f *fakeKernel) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = nil
	f.removePins = nil
	f.tcDetaches = nil
	f.tcFilters = make(map[tcFilterKey]uint32)
	f.failOnProgram = make(map[string]error)
	f.failOnAttach = make(map[string]error)
	f.failOnDetach = make(map[uint32]error)
	f.failOnIfname = make(map[string]error)
	f.failOnIfindex = make(map[int]error)
	f.failOnNthLoad = 0
	f.loadCount = 0
}

func (f *fakeKernel) Load(_ context.Context, spec bpfman.LoadSpec, bpffsRoot bpffs.Root) (bpfman.LoadOutput, error) {
	// Validate program type - mirrors real kernel behaviour
	if spec.ProgramType() == bpfman.ProgramTypeUnspecified {
		err := fmt.Errorf("program type must be specified")
		f.recordOp("load", spec.ProgramName(), 0, err)
		return bpfman.LoadOutput{}, err
	}
	if spec.ProgramType() < bpfman.ProgramTypeXDP || spec.ProgramType() > bpfman.ProgramTypeFexit {
		err := fmt.Errorf("invalid program type: %d", spec.ProgramType())
		f.recordOp("load", spec.ProgramName(), 0, err)
		return bpfman.LoadOutput{}, err
	}

	// Check error injection
	f.mu.Lock()
	f.loadCount++
	loadNum := f.loadCount
	failErr := f.failOnProgram[spec.ProgramName()]
	failOnNth := f.failOnNthLoad
	f.mu.Unlock()

	if failErr != nil {
		f.recordOp("load", spec.ProgramName(), 0, failErr)
		return bpfman.LoadOutput{}, failErr
	}
	if failOnNth > 0 && loadNum == failOnNth {
		err := fmt.Errorf("injected error on load %d", loadNum)
		f.recordOp("load", spec.ProgramName(), 0, err)
		return bpfman.LoadOutput{}, err
	}

	id := f.nextID.Add(1)
	// Compute paths the same way the real kernel does - using kernel ID
	progPinPath := fmt.Sprintf("%s/prog_%d", bpffsRoot, id)

	// Map sharing: if MapOwnerID is set, use the owner's maps directory
	var mapsDir string
	if spec.MapOwnerID() != 0 {
		// Share maps with the owner program
		mapsDir = fmt.Sprintf("%s/maps/%d", bpffsRoot, spec.MapOwnerID())
	} else {
		// Own maps - use our kernel ID
		mapsDir = fmt.Sprintf("%s/maps/%d", bpffsRoot, id)
	}

	// Create the pin file on disk so that GC's ownership check
	// (os.Stat on the pin path) recognises this as our program.
	if err := os.MkdirAll(string(bpffsRoot), 0755); err != nil {
		return bpfman.LoadOutput{}, fmt.Errorf("fake kernel: mkdir pin dir: %w", err)
	}
	if err := os.WriteFile(progPinPath, nil, 0644); err != nil {
		return bpfman.LoadOutput{}, fmt.Errorf("fake kernel: create pin file: %w", err)
	}

	fp := fakeProgram{
		id:          id,
		name:        spec.ProgramName(),
		programType: spec.ProgramType(),
		pinPath:     progPinPath,
		pinDir:      mapsDir,
	}
	f.programs[id] = fp
	f.recordOp("load", spec.ProgramName(), id, nil)
	return bpfman.LoadOutput{
		PinPath:      fp.pinPath,
		MapsDir:      fp.pinDir,
		License:      "GPL",
		InferredType: fp.programType,
		Program: &kernel.Program{
			ID:            fp.id,
			Name:          fp.name,
			ProgramType:   kernel.NewProgramType(fp.programType.String()),
			GPLCompatible: true,
		},
	}, nil
}

func (f *fakeKernel) Unload(_ context.Context, pinPath string) error {
	for id, p := range f.programs {
		// Match by either program pin path or maps directory
		if p.pinPath == pinPath || p.pinDir == pinPath {
			delete(f.programs, id)
			f.recordOp("unload", p.name, id, nil)
			return nil
		}
	}
	return nil
}

func (f *fakeKernel) UnloadProgram(_ context.Context, progPinPath, mapsDir string) error {
	// Fake implementation - just removes any program whose pin path matches
	for id, p := range f.programs {
		if p.pinPath == progPinPath || p.pinDir == mapsDir {
			delete(f.programs, id)
			f.recordOp("unload", p.name, id, nil)
			return nil
		}
	}
	return nil
}

// ProgramCount returns the number of programs currently loaded.
func (f *fakeKernel) ProgramCount() int {
	return len(f.programs)
}

func (f *fakeKernel) LinkCount() int {
	return len(f.links)
}

func (f *fakeKernel) Programs(_ context.Context) iter.Seq2[kernel.Program, error] {
	return func(yield func(kernel.Program, error) bool) {
		for id, p := range f.programs {
			kp := kernel.Program{
				ID:          id,
				Name:        p.name,
				ProgramType: kernel.NewProgramType(p.programType.String()),
			}
			if !yield(kp, nil) {
				return
			}
		}
	}
}

func (f *fakeKernel) GetProgramByID(_ context.Context, id uint32) (kernel.Program, error) {
	p, ok := f.programs[id]
	if !ok {
		return kernel.Program{}, fmt.Errorf("program %d not found", id)
	}
	return kernel.Program{
		ID:          id,
		Name:        p.name,
		ProgramType: kernel.NewProgramType(p.programType.String()),
	}, nil
}

func (f *fakeKernel) GetLinkByID(_ context.Context, id uint32) (kernel.Link, error) {
	link, ok := f.links[id]
	if !ok {
		return kernel.Link{}, fmt.Errorf("link %d not found", id)
	}
	return kernel.Link{
		ID:        id,
		LinkType:  string(link.Spec.Kind),
		ProgramID: 0, // fakeKernel doesn't track program association
	}, nil
}

func (f *fakeKernel) GetMapByID(_ context.Context, id uint32) (kernel.Map, error) {
	// fakeKernel doesn't track maps, return a minimal stub
	return kernel.Map{ID: id}, nil
}

func (f *fakeKernel) Maps(_ context.Context) iter.Seq2[kernel.Map, error] {
	return func(yield func(kernel.Map, error) bool) {}
}

func (f *fakeKernel) Links(_ context.Context) iter.Seq2[kernel.Link, error] {
	return func(yield func(kernel.Link, error) bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for id := range f.links {
			kl := kernel.Link{
				ID: id,
			}
			if !yield(kl, nil) {
				return
			}
		}
	}
}

func (f *fakeKernel) ListPinDir(_ context.Context, pinDir string, includeMaps bool) (*kernel.PinDirContents, error) {
	return &kernel.PinDirContents{}, nil
}

func (f *fakeKernel) GetPinned(_ context.Context, pinPath string) (*kernel.PinnedProgram, error) {
	return nil, nil
}

func (f *fakeKernel) AttachTracepoint(_ context.Context, progPinPath, group, name, linkPinPath string) (bpfman.AttachOutput, error) {
	// Check error injection
	f.mu.Lock()
	failErr := f.failOnAttach["tracepoint"]
	f.mu.Unlock()
	if failErr != nil {
		f.recordOp("attach", "tracepoint:"+group+"/"+name, 0, failErr)
		return bpfman.AttachOutput{}, failErr
	}

	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "tracepoint"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindTracepoint,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.TracepointDetails{Group: group, Name: name},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	f.recordOp("attach", "tracepoint:"+group+"/"+name, id, nil)
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) AttachXDP(_ context.Context, progPinPath string, ifindex int, linkPinPath string) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "xdp"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindXDP,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.XDPDetails{Ifindex: uint32(ifindex)},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) AttachKprobe(_ context.Context, progPinPath, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	linkKind := bpfman.LinkKindKprobe
	kernelLinkType := "kprobe"
	if retprobe {
		linkKind = bpfman.LinkKindKretprobe
		kernelLinkType = "kretprobe"
	}
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: kernelLinkType}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      linkKind,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.KprobeDetails{FnName: fnName, Offset: offset, Retprobe: retprobe},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) AttachUprobeLocal(_ context.Context, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	linkKind := bpfman.LinkKindUprobe
	kernelLinkType := "uprobe"
	if retprobe {
		linkKind = bpfman.LinkKindUretprobe
		kernelLinkType = "uretprobe"
	}
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: kernelLinkType}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      linkKind,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.UprobeDetails{Target: target, FnName: fnName, Offset: offset, Retprobe: retprobe, ContainerPid: 0},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) AttachUprobeContainer(_ context.Context, _ lock.WriterScope, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string, containerPid int32) (bpfman.AttachOutput, error) {
	// Container uprobes are synthetic - they use perf_event and have no kernel link
	id := bpfman.SyntheticLinkIDBase + f.nextID.Add(1)
	linkKind := bpfman.LinkKindUprobe
	if retprobe {
		linkKind = bpfman.LinkKindUretprobe
	}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      linkKind,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.UprobeDetails{Target: target, FnName: fnName, Offset: offset, Retprobe: retprobe, ContainerPid: containerPid},
		},
		Status: bpfman.LinkStatus{
			Kernel:     nil, // No kernel link for perf_event-based uprobes
			KernelSeen: false,
			PinPresent: false, // Container uprobes can't be pinned
		},
	}
	f.links[id] = &link
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: nil, // No kernel link for perf_event-based uprobes
		PinPath:    linkPinPath,
		Synthetic:  true,
	}, nil
}

func (f *fakeKernel) AttachFentry(_ context.Context, progPinPath, fnName, linkPinPath string) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "fentry"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindFentry,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.FentryDetails{FnName: fnName},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) AttachFexit(_ context.Context, progPinPath, fnName, linkPinPath string) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "fexit"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindFexit,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.FexitDetails{FnName: fnName},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) DetachLink(_ context.Context, linkPinPath string) error {
	for id, link := range f.links {
		pinPath := ""
		if link.Spec.PinPath != nil {
			pinPath = link.Spec.PinPath.String()
		}
		if pinPath == linkPinPath {
			// Check error injection
			f.mu.Lock()
			failErr := f.failOnDetach[id]
			f.mu.Unlock()
			if failErr != nil {
				f.recordOp("detach", linkPinPath, id, failErr)
				return failErr
			}
			delete(f.links, id)
			f.recordOp("detach", linkPinPath, id, nil)
			return nil
		}
	}
	// Link not found - still record the detach attempt
	f.recordOp("detach", linkPinPath, 0, nil)
	return nil
}

func (f *fakeKernel) AttachXDPDispatcher(_ context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*interpreter.XDPDispatcherResult, error) {
	// Check for interface-specific failure injection
	f.mu.Lock()
	if err, ok := f.failOnIfindex[spec.Target.IfIndex]; ok {
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()

	dispatcherID := f.nextID.Add(1)
	linkID := f.nextID.Add(1)
	// Add dispatcher program to programs map so GC sees it as valid
	f.programs[dispatcherID] = fakeProgram{
		id:          dispatcherID,
		name:        "xdp_dispatcher",
		programType: bpfman.ProgramTypeXDP,
		pinPath:     spec.ProgPinPath,
	}
	return &interpreter.XDPDispatcherResult{
		DispatcherID:  dispatcherID,
		LinkID:        linkID,
		DispatcherPin: spec.ProgPinPath,
		LinkPin:       spec.LinkPinPath,
	}, nil
}

func (f *fakeKernel) AttachXDPExtension(_ context.Context, spec dispatcher.XDPExtensionAttachSpec) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "xdp"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindXDP,
			PinPath:   bpffs.NewLinkPath(spec.LinkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.XDPDetails{Position: int32(spec.Position)},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: spec.LinkPinPath != "",
		},
	}
	f.links[id] = &link
	// Record the operation with mapPinDir for test verification
	f.recordExtensionAttach("attach-xdp-ext", spec.ProgramName, id, spec.MapPinDir)
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    spec.LinkPinPath,
	}, nil
}

func (f *fakeKernel) AttachTCDispatcher(_ context.Context, spec dispatcher.TCDispatcherAttachSpec) (*interpreter.TCDispatcherResult, error) {
	// Check for interface-specific failure injection
	f.mu.Lock()
	if err, ok := f.failOnIfname[spec.IfName]; ok {
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()

	dispatcherID := f.nextID.Add(1)
	handle := f.nextID.Add(1)
	// Add dispatcher program to programs map so GC sees it as valid
	f.programs[dispatcherID] = fakeProgram{
		id:          dispatcherID,
		name:        "tc_dispatcher",
		programType: bpfman.ProgramTypeTC,
		pinPath:     spec.ProgPinPath,
	}

	// Determine parent handle from direction
	var parent uint32
	switch spec.Direction {
	case bpfman.TCDirectionIngress:
		parent = 0xFFFFFFF2 // netlink.HANDLE_MIN_INGRESS
	case bpfman.TCDirectionEgress:
		parent = 0xFFFFFFF3 // netlink.HANDLE_MIN_EGRESS
	}

	// Store TC filter so FindTCFilterHandle can look it up
	f.mu.Lock()
	f.tcFilters[tcFilterKey{ifindex: spec.Target.IfIndex, parent: parent, priority: 50}] = handle
	f.mu.Unlock()

	return &interpreter.TCDispatcherResult{
		DispatcherID:  dispatcherID,
		DispatcherPin: spec.ProgPinPath,
		Handle:        handle,
		Priority:      50,
	}, nil
}

func (f *fakeKernel) DetachTCFilter(_ context.Context, ifindex int, ifname string, parent uint32, priority uint16, handle uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tcFilterKey{ifindex: ifindex, parent: parent, priority: priority}
	f.tcDetaches = append(f.tcDetaches, key)
	delete(f.tcFilters, key)
	return nil
}

func (f *fakeKernel) FindTCFilterHandle(_ context.Context, ifindex int, parent uint32, priority uint16) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tcFilterKey{ifindex: ifindex, parent: parent, priority: priority}
	handle, ok := f.tcFilters[key]
	if !ok {
		return 0, fmt.Errorf("no TC filter for ifindex=%d parent=%x priority=%d", ifindex, parent, priority)
	}
	return handle, nil
}

func (f *fakeKernel) AttachTCExtension(_ context.Context, spec dispatcher.TCExtensionAttachSpec) (bpfman.AttachOutput, error) {
	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "tc"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindTC,
			PinPath:   bpffs.NewLinkPath(spec.LinkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.TCDetails{Position: int32(spec.Position)},
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: spec.LinkPinPath != "",
		},
	}
	f.links[id] = &link
	// Record the operation with mapPinDir for test verification
	f.recordExtensionAttach("attach-tc-ext", spec.ProgramName, id, spec.MapPinDir)
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    spec.LinkPinPath,
	}, nil
}

func (f *fakeKernel) AttachTCX(_ context.Context, ifindex int, direction, programPinPath, linkPinPath, netns string, order bpfman.TCXAttachOrder) (bpfman.AttachOutput, error) {
	// Check for interface-specific failure injection
	f.mu.Lock()
	if err, ok := f.failOnIfindex[ifindex]; ok {
		f.mu.Unlock()
		return bpfman.AttachOutput{}, err
	}
	f.mu.Unlock()

	id := f.nextID.Add(1)
	kl := kernel.Link{ID: id, ProgramID: 0, LinkType: "tcx"}
	// Store for DetachLink lookup and kernel iteration
	link := bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(id),
			Kind:      bpfman.LinkKindTCX,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
		},
		Status: bpfman.LinkStatus{
			Kernel:     &kl,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}
	f.links[id] = &link
	// Record the operation with programPinPath for test verification
	f.recordTCXAttach(programPinPath, id)
	return bpfman.AttachOutput{
		LinkID:     id,
		KernelLink: &kl,
		PinPath:    linkPinPath,
	}, nil
}

func (f *fakeKernel) RemovePin(_ context.Context, path string) error {
	f.mu.Lock()
	f.removePins = append(f.removePins, path)
	f.mu.Unlock()

	// Remove programs matching this pin path (for dispatcher cleanup)
	for id, prog := range f.programs {
		if prog.pinPath == path {
			delete(f.programs, id)
			break
		}
	}
	return nil
}

// RemovedPins returns a copy of all paths passed to RemovePin.
func (f *fakeKernel) RemovedPins() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]string, len(f.removePins))
	copy(result, f.removePins)
	return result
}

// TCDetaches returns a copy of all TC filter detach operations.
func (f *fakeKernel) TCDetaches() []tcFilterKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]tcFilterKey, len(f.tcDetaches))
	copy(result, f.tcDetaches)
	return result
}

// TCFilterCount returns the number of TC filters currently tracked.
func (f *fakeKernel) TCFilterCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tcFilters)
}

func (f *fakeKernel) RepinMap(_ context.Context, srcPath, dstPath string) error {
	return nil // Fake implementation - no-op
}
