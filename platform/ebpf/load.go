package ebpf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cilium/ebpf"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
)

// Load loads a BPF program into the kernel.
//
// Load loads a BPF program and pins it using program ID-based paths.
//
// Pin paths follow the upstream bpfman convention, computed via bpffs methods:
//   - Program: bpffs.ProgPinPath(program_id)
//   - Maps: bpffs.MapPinDir(program_id)/<map_name>
//
// On failure, all successfully pinned objects are cleaned up.
//
// Map sharing: If spec.MapOwnerID() is non-zero, this program will share maps
// with the owner program instead of creating its own. The owner's maps directory
// must exist and contain the required pinned maps. This is used when loading
// multiple programs from the same image (e.g., via the bpfman-operator) where
// all programs should share the same map instances.
func (k *kernelAdapter) Load(ctx context.Context, spec bpfman.LoadSpec, bpffs fs.BPFFS) (bpfman.LoadOutput, error) {
	// Load the collection from the object file
	collSpec, err := ebpf.LoadCollectionSpec(spec.ObjectPath())
	if err != nil {
		return bpfman.LoadOutput{}, fmt.Errorf("failed to load collection spec: %w", err)
	}

	// Set global data if provided
	for name, data := range spec.GlobalData() {
		if v, ok := collSpec.Variables[name]; ok {
			if err := v.Set(data); err != nil {
				return bpfman.LoadOutput{}, fmt.Errorf("set variable %q: %w", name, err)
			}
		}
	}

	// Record which maps declare PinByName before clearing the flag.
	// These maps will be shared across programs via a shared pin
	// directory, matching aya's LIBBPF_PIN_BY_NAME behaviour.
	pinByNameMaps := make(map[string]bool)
	for name, mapSpec := range collSpec.Maps {
		if mapSpec.Pinning == ebpf.PinByName && !strings.HasPrefix(name, ".") {
			pinByNameMaps[name] = true
		}
	}

	// Clear map pinning flags - we'll pin manually after getting the kernel ID.
	// Some BPF programs have maps annotated with PIN_BY_NAME which requires
	// a pin path at load time, but we need the kernel ID first.
	for _, mapSpec := range collSpec.Maps {
		mapSpec.Pinning = ebpf.PinNone
	}

	// Find the requested program and get its license (needed before loading)
	progSpec, ok := collSpec.Programs[spec.ProgramName()]
	if !ok {
		available := make([]string, 0, len(collSpec.Programs))
		for name := range collSpec.Programs {
			available = append(available, name)
		}
		sort.Strings(available)
		return bpfman.LoadOutput{}, fmt.Errorf("program %q not found in collection spec; available programs: %v", spec.ProgramName(), available)
	}
	license := progSpec.License

	// Determine program type: prefer user-specified type, fall back to ELF inference.
	// The user's CLI specification (e.g., --programs kretprobe:func) takes precedence
	// because a kprobe program CAN be attached as either entry or return probe.
	programType := spec.ProgramType()
	secInferredType := inferProgramType(progSpec.SectionName)
	if programType == (bpfman.ProgramType{}) {
		// Fall back to inferring from ELF section name
		programType = secInferredType
	}

	// fentry/fexit/lsm tracing programs are bound to their target
	// kernel function and to BPF_TRACE_{FENTRY,FEXIT} at LOAD
	// time via expected_attach_type. If the caller asks for one
	// type but the .bpf.o was compiled with the SEC of the other,
	// the kernel will load the program according to SEC and
	// silently ignore the caller's intent. Surface that as a
	// hard error: any subsequent metadata bpfman records about
	// the program would be a lie, and the program would attach
	// at the wrong site (fentry vs fexit affects retval access
	// and verifier rules). Kprobe/kretprobe deliberately remain
	// interchangeable here because the entry/return distinction
	// is set at perf_event_open time, not at load.
	if programType.RequiresAttachFunc() &&
		secInferredType != (bpfman.ProgramType{}) &&
		secInferredType.RequiresAttachFunc() &&
		programType != secInferredType {
		return bpfman.LoadOutput{}, fmt.Errorf(
			"program type mismatch: caller specified %s but ELF section %q implies %s; "+
				"recompile the .bpf.o with the matching SEC or pass the matching ProgramType",
			programType, progSpec.SectionName, secInferredType)
	}

	// For fentry/fexit/lsm and other tracing programs that
	// require an attach function name, propagate the spec's
	// AttachFunc into ProgramSpec.AttachTo so the load-time
	// attach target overrides whatever the ELF SEC name said.
	// Without this, a program compiled with
	// SEC("fexit/some_placeholder") loads bound to that
	// placeholder regardless of the AttachFunc the caller passes
	// at attach time -- the kernel ties tracing programs to their
	// target at LOAD, not at link-create. cilium/ebpf resolves
	// AttachTo through vmlinux + loaded module BTF, so this also
	// makes fentry/fexit work against kernel-module functions
	// (e.g. the bpfman_e2e_targets slot pool used by the
	// hermetic e2e tests).
	if programType.RequiresAttachFunc() && spec.AttachFunc() != "" {
		progSpec.AttachTo = spec.AttachFunc()
	}

	// For XDP/TC programs: load as BPF_PROG_TYPE_EXT targeting a test
	// dispatcher. This matches Rust bpfman's approach where extension
	// programs are loaded once and reused from their pin on every
	// dispatcher rebuild, rather than re-reading the ELF file.
	if programType == bpfman.ProgramTypeXDP || programType == bpfman.ProgramTypeTC {
		var testProg *ebpf.Program
		if programType == bpfman.ProgramTypeXDP {
			testProg, err = k.testDisp.getXDP()
		} else {
			testProg, err = k.testDisp.getTC()
		}
		if err != nil {
			return bpfman.LoadOutput{}, fmt.Errorf("get test dispatcher for %s: %w", programType, err)
		}
		progSpec.Type = ebpf.Extension
		progSpec.AttachTarget = testProg
		progSpec.AttachTo = "prog0"
	}

	// Check if we should share maps with another program (map_owner_id).
	// When set, we load the owner's pinned maps and pass them as replacements
	// so this program uses the same map instances.
	var mapReplacements map[string]*ebpf.Map
	var ownerMapsDir string
	mapOwnerID := spec.MapOwnerID()

	if mapOwnerID != 0 {
		ownerMapsDir = bpffs.MapPinDir(mapOwnerID)
		mapReplacements = make(map[string]*ebpf.Map)

		k.logger.Debug("loading shared maps from owner program",
			"map_owner_id", mapOwnerID,
			"owner_maps_dir", ownerMapsDir)

		// Load pinned maps from owner's directory.
		// We iterate over collSpec.Maps to get the exact ELF map names.
		for name := range collSpec.Maps {
			// Skip internal maps (same filtering as pinning below)
			if strings.HasPrefix(name, ".") {
				continue
			}
			mapPath := bpffs.MapPinPath(mapOwnerID, name)
			m, err := ebpf.LoadPinnedMap(mapPath.String(), nil)
			if err != nil {
				// Clean up any maps we've already loaded
				for _, loaded := range mapReplacements {
					loaded.Close()
				}
				return bpfman.LoadOutput{}, fmt.Errorf("load shared map %q from owner %d: %w", name, mapOwnerID, err)
			}
			mapReplacements[name] = m
			k.logger.Debug("loaded shared map from owner", "name", name, "path", mapPath)
		}
	}

	// For PinByName maps without an explicit owner, check the shared
	// pin directory for existing maps. If found, use them as
	// replacements so this program shares the same kernel map
	// instances as previous loads.
	if len(pinByNameMaps) > 0 && mapOwnerID == 0 {
		for name := range pinByNameMaps {
			sharedPath := bpffs.SharedMapPin(name)
			m, err := ebpf.LoadPinnedMap(sharedPath.String(), nil)
			if err != nil {
				continue // not yet pinned; will create after load
			}
			if mapReplacements == nil {
				mapReplacements = make(map[string]*ebpf.Map)
			}
			mapReplacements[name] = m
			k.logger.Debug("loaded shared PinByName map", "name", name, "path", sharedPath)
		}
	}

	// Load collection - use map replacements if sharing with owner
	var coll *ebpf.Collection
	if len(mapReplacements) > 0 {
		coll, err = ebpf.NewCollectionWithOptions(collSpec, ebpf.CollectionOptions{
			MapReplacements: mapReplacements,
		})
	} else {
		coll, err = ebpf.NewCollection(collSpec)
	}
	if err != nil {
		// Clean up map replacements on error
		for _, m := range mapReplacements {
			m.Close()
		}
		return bpfman.LoadOutput{}, fmt.Errorf("failed to load collection: %w", err)
	}
	defer coll.Close()

	prog, ok := coll.Programs[spec.ProgramName()]
	if !ok {
		return bpfman.LoadOutput{}, fmt.Errorf("program %q not found in collection", spec.ProgramName())
	}

	// Get program info to obtain kernel ID
	info, err := prog.Info()
	if err != nil {
		return bpfman.LoadOutput{}, fmt.Errorf("failed to get program info: %w", err)
	}
	progID, ok := info.ID()
	if !ok {
		return bpfman.LoadOutput{}, fmt.Errorf("failed to get program ID from kernel")
	}
	programID := kernel.ProgramID(progID)

	// Track pinned paths for rollback on failure.
	// Use BPFFS safe removal to ensure we only remove paths under the bpffs mount.
	var pinnedPaths []string
	cleanup := func() {
		for i := len(pinnedPaths) - 1; i >= 0; i-- {
			if err := bpffs.SafeRemove(pinnedPaths[i]); err != nil {
				k.logger.Warn("failed to remove pin during cleanup", "path", pinnedPaths[i], "error", err)
			}
		}
	}

	// Pin program using bpffs convention
	progPinPath := bpffs.ProgPinPath(programID)
	if err := prog.Pin(progPinPath.String()); err != nil {
		return bpfman.LoadOutput{}, fmt.Errorf("failed to pin program: %w", err)
	}
	pinnedPaths = append(pinnedPaths, progPinPath.String())

	// Determine the maps directory to use:
	// - If sharing maps (map_owner_id set): use owner's mapsDir, don't create/pin maps
	// - Otherwise: create our own mapsDir and pin maps
	var mapsDir string
	if mapOwnerID != 0 {
		// Use owner's maps directory - maps are already pinned there
		mapsDir = ownerMapsDir
		k.logger.Debug("using shared maps from owner",
			"program_id", programID,
			"map_owner_id", mapOwnerID,
			"maps_dir", mapsDir)
	} else {
		// Create our own maps directory using bpffs convention
		mapsDir = bpffs.MapPinDir(programID)
		if err := bpffs.EnsureMapsDir(programID); err != nil {
			cleanup()
			return bpfman.LoadOutput{}, fmt.Errorf("failed to create maps directory: %w", err)
		}

		// Pin PinByName maps to the shared directory (first load
		// creates the pin; subsequent loads reused it above via
		// mapReplacements).
		if len(pinByNameMaps) > 0 {
			if err := bpffs.EnsureSharedMapPinDir(); err != nil {
				cleanup()
				return bpfman.LoadOutput{}, fmt.Errorf("failed to create shared map pin directory: %w", err)
			}
			for name := range pinByNameMaps {
				if mapReplacements[name] != nil {
					continue // already pinned at shared location
				}
				m := coll.Maps[name]
				if m == nil {
					continue
				}
				sharedPath := bpffs.SharedMapPin(name)
				if err := m.Pin(sharedPath.String()); err != nil {
					cleanup()
					if rmErr := bpffs.SafeRemoveAll(mapsDir); rmErr != nil {
						k.logger.Warn("failed to remove maps directory during cleanup", "path", mapsDir, "error", rmErr)
					}
					return bpfman.LoadOutput{}, fmt.Errorf("failed to pin shared map %q: %w", name, err)
				}
				pinnedPaths = append(pinnedPaths, sharedPath.String())
				k.logger.Debug("pinned PinByName map to shared directory", "name", name, "path", sharedPath)
			}
		}

		// Pin all maps to the per-program directory. Maps that
		// are already pinned (at the shared location) need to be
		// cloned first, since Clone() produces an unpinned
		// duplicate that can be pinned to a second path.
		for name, m := range coll.Maps {
			if strings.HasPrefix(name, ".") {
				continue
			}
			mapPinPath := bpffs.MapPinPath(programID, name)
			if m.IsPinned() {
				clone, cloneErr := m.Clone()
				if cloneErr != nil {
					cleanup()
					if rmErr := bpffs.SafeRemoveAll(mapsDir); rmErr != nil {
						k.logger.Warn("failed to remove maps directory during cleanup", "path", mapsDir, "error", rmErr)
					}
					return bpfman.LoadOutput{}, fmt.Errorf("failed to clone map %q for per-program pin: %w", name, cloneErr)
				}
				if err := clone.Pin(mapPinPath.String()); err != nil {
					clone.Close()
					cleanup()
					if rmErr := bpffs.SafeRemoveAll(mapsDir); rmErr != nil {
						k.logger.Warn("failed to remove maps directory during cleanup", "path", mapsDir, "error", rmErr)
					}
					return bpfman.LoadOutput{}, fmt.Errorf("failed to pin map %q: %w", name, err)
				}
				clone.Close()
			} else {
				if err := m.Pin(mapPinPath.String()); err != nil {
					cleanup()
					if rmErr := bpffs.SafeRemoveAll(mapsDir); rmErr != nil {
						k.logger.Warn("failed to remove maps directory during cleanup", "path", mapsDir, "error", rmErr)
					}
					return bpfman.LoadOutput{}, fmt.Errorf("failed to pin map %q: %w", name, err)
				}
			}
			pinnedPaths = append(pinnedPaths, mapPinPath.String())
		}
	}

	ebpfMapIDs, ok := info.MapIDs()
	if !ok {
		cleanup()
		if mapOwnerID == 0 {
			if rmErr := bpffs.SafeRemoveAll(mapsDir); rmErr != nil {
				k.logger.Warn("failed to remove maps directory during cleanup", "path", mapsDir, "error", rmErr)
			}
		}
		return bpfman.LoadOutput{}, fmt.Errorf("failed to get map IDs from kernel")
	}
	_ = ebpfMapIDs // MapIDs now accessed via kernel.Program

	// Collect PinByName map names for reference counting.
	var sharedMapNames []string
	for name := range pinByNameMaps {
		sharedMapNames = append(sharedMapNames, name)
	}
	sort.Strings(sharedMapNames)

	return bpfman.LoadOutput{
		PinPath:        progPinPath,
		MapsDir:        mapsDir,
		Program:        ToKernelProgram(info, license),
		License:        license,
		InferredType:   programType,
		SharedMapNames: sharedMapNames,
	}, nil
}

// Unload removes a BPF program from the kernel by unpinning.
// Handles both old-style (directory containing everything) and new-style
// (separate program pin and maps directory) layouts.
func (k *kernelAdapter) Unload(ctx context.Context, pinPath string) error {
	info, err := os.Stat(pinPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat pin path: %w", err)
	}

	// If it's a file (program pin), just remove it
	if !info.IsDir() {
		if err := os.Remove(pinPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to unpin %s: %w", pinPath, err)
		}
		return nil
	}

	// It's a directory - remove contents then directory
	entries, err := os.ReadDir(pinPath)
	if err != nil {
		return fmt.Errorf("failed to read pin directory: %w", err)
	}

	for _, e := range entries {
		path := filepath.Join(pinPath, e.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to unpin %s: %w", path, err)
		}
	}

	if err := os.Remove(pinPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove pin directory: %w", err)
	}

	return nil
}

// UnloadProgram removes a program and its maps using the upstream pin layout.
// progPinPath is the program pin (e.g., /run/bpfman/fs/prog_123)
// mapsDir is the maps directory (e.g., /run/bpfman/fs/maps/123)
func (k *kernelAdapter) UnloadProgram(ctx context.Context, progPinPath bpfman.ProgPinPath, mapsDir string) error {
	// Remove program pin
	if err := os.Remove(progPinPath.String()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to unpin program %s: %w", progPinPath, err)
	}

	// Remove maps directory and contents
	if mapsDir != "" {
		if err := k.Unload(ctx, mapsDir); err != nil {
			return fmt.Errorf("failed to unload maps: %w", err)
		}
	}

	return nil
}
