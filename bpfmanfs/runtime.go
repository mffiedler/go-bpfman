package bpfmanfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	bytecodeName = "bytecode.o"
	provName     = "provenance.json"
	dirMode      = os.FileMode(0755)
	fileMode     = os.FileMode(0644)

	programsDir = "programs"
	stagingDir  = ".staging"
)

// Provenance records how a bytecode file was obtained. Written
// alongside bytecode.o as a diagnostic trace; never read on
// operational code paths.
type Provenance struct {
	Version     int       `json:"version"`
	KernelID    uint32    `json:"kernel_id"`
	ProgramName string    `json:"program_name"`
	Source      string    `json:"source"`
	SourceKind  string    `json:"source_kind"` // "file", "image", "unknown"
	LoadedAt    time.Time `json:"loaded_at"`   // RFC 3339 UTC
}

// BytecodeFS provides regular-filesystem operations for bytecode
// persistence. Fields are unexported; obtain via FSLayout.BytecodeFS().
type BytecodeFS struct {
	layout FSLayout
}

// Valid reports whether the BytecodeFS was obtained from a valid Layout.
func (rt BytecodeFS) Valid() bool {
	return rt.layout.Valid()
}

// mustValid panics if rt was not obtained from FSLayout.BytecodeFS().
func (rt BytecodeFS) mustValid() {
	if !rt.Valid() {
		panic("bpfmanfs: zero BytecodeFS used; obtain via FSLayout.BytecodeFS()")
	}
}

// FilesystemContext is a capability token proving that the filesystem
// directories exist and bpffs is mounted. Obtain via runtime.New().
//
// Holding a FilesystemContext guarantees:
//   - Base directory and subdirectories exist
//   - bpffs is mounted at the expected mount point
//
// This enables upfront validation before operations begin.
type FilesystemContext struct {
	layout FSLayout
}

// NewFilesystemContext creates a FilesystemContext from a validated FSLayout.
// This is called by runtime.New() after directories and bpffs are ready.
// Direct callers must ensure the filesystem is properly initialised.
func NewFilesystemContext(layout FSLayout) FilesystemContext {
	return FilesystemContext{layout: layout}
}

// Layout returns the underlying FSLayout.
func (r FilesystemContext) Layout() FSLayout {
	return r.layout
}

// BPFFS returns the bpffs accessor for pin path conventions.
func (r FilesystemContext) BPFFS() BPFFS {
	return r.layout.BPFFS()
}

// BytecodeFS returns the bytecode filesystem accessor for program persistence.
func (r FilesystemContext) BytecodeFS() BytecodeFS {
	return r.layout.BytecodeFS()
}

// Valid reports whether the FilesystemContext was properly constructed.
func (r FilesystemContext) Valid() bool {
	return r.layout.Valid()
}

// programsPath returns <base>/programs.
func (rt BytecodeFS) programsPath() string {
	return filepath.Join(rt.layout.base, programsDir)
}

// stagingPath returns <base>/.staging.
func (rt BytecodeFS) stagingPath() string {
	return filepath.Join(rt.layout.base, stagingDir)
}

// programDir returns <base>/programs/{id}.
func (rt BytecodeFS) programDir(id uint32) string {
	return filepath.Join(rt.layout.base, programsDir, strconv.FormatUint(uint64(id), 10))
}

// PublishBytecode publishes srcPath to:
//
//	<base>/programs/{id}/bytecode.o
//
// via staging under <base>/.staging/.
//
// srcPath must refer to a readable regular file containing the ELF
// object. If it does not exist or is not readable, a PathError is
// returned.
//
// If <base>/programs/{id} already exists, PublishBytecode returns
// ErrFinalExists. The caller is expected to have run GC before
// loading, which removes orphan directories. An existing final
// directory after GC indicates an invariant violation.
//
// A provenance.json is written alongside the bytecode. Publish is
// atomic (rename on the same filesystem).
func (rt BytecodeFS) PublishBytecode(id uint32, srcPath string, prov Provenance) error {
	rt.mustValid()

	// Validate source file.
	if err := validateRegularFile(srcPath); err != nil {
		return err
	}

	finalDir := rt.programDir(id)
	programs := rt.programsPath()
	staging := rt.stagingPath()

	// Check final directory does not exist.
	var exists bool
	if err := (statExistsOp{path: finalDir, exists: &exists}).exec(); err != nil {
		return &PathError{Op: "publish", Path: finalDir, Err: err}
	}
	if exists {
		return fmt.Errorf("%w: %s", ErrFinalExists, finalDir)
	}

	// Ensure parent directories exist.
	if err := osInterp([]op{
		mkdirAllOp{path: programs, perm: dirMode},
		mkdirAllOp{path: staging, perm: dirMode},
	}); err != nil {
		return &PathError{Op: "publish", Path: programs, Err: err}
	}

	// Create temp dir under staging for atomic publish.
	var tmpDir string
	if err := (mkdirTempOp{dir: staging, pattern: "pub-*", result: &tmpDir}).exec(); err != nil {
		return &PathError{Op: "publish", Path: staging, Err: err}
	}

	// Clean up temp dir on any error after this point.
	cleanup := true
	defer func() {
		if cleanup {
			_ = safeRemoveAll(staging, tmpDir)
		}
	}()

	// Copy bytecode and write provenance into temp dir.
	bytecodeDst := filepath.Join(tmpDir, bytecodeName)
	provDst := filepath.Join(tmpDir, provName)

	if err := osInterp([]op{
		copyFileOp{src: srcPath, dst: bytecodeDst, perm: fileMode},
		writeJSONOp{path: provDst, perm: fileMode, v: prov},
	}); err != nil {
		return &PathError{Op: "publish", Path: tmpDir, Err: err}
	}

	// Atomic rename from staging to final location.
	if err := (renameOp{oldpath: tmpDir, newpath: finalDir}).exec(); err != nil {
		return &PathError{Op: "publish", Path: finalDir, Err: err}
	}

	cleanup = false
	return nil
}

// RemoveProgram removes <base>/programs/{id}/ and its contents.
// Returns nil if the directory does not exist. Uses safeRemoveAll to
// verify the target is under the programs directory.
func (rt BytecodeFS) RemoveProgram(id uint32) error {
	rt.mustValid()
	return safeRemoveAll(rt.programsPath(), rt.programDir(id))
}

// RemoveProgramDir removes a program directory by path. The path must
// be a direct child of <base>/programs/. Returns nil if the directory
// does not exist. This handles both numeric and non-numeric directory
// names (e.g., orphaned directories with unexpected names).
func (rt BytecodeFS) RemoveProgramDir(path string) error {
	rt.mustValid()
	return safeRemoveAll(rt.programsPath(), path)
}

// ProgramExists reports whether <base>/programs/{id}/ exists.
func (rt BytecodeFS) ProgramExists(id uint32) bool {
	rt.mustValid()
	var exists bool
	_ = (statExistsOp{path: rt.programDir(id), exists: &exists}).exec()
	return exists
}

// ProgramBytecodePath returns the published bytecode path for DB
// ObjectPath storage.
func (rt BytecodeFS) ProgramBytecodePath(id uint32) string {
	rt.mustValid()
	return filepath.Join(rt.programDir(id), bytecodeName)
}

// CleanStaging removes all entries under <base>/.staging/. Staging is
// a writer-only concern and is never visible to readers.
func (rt BytecodeFS) CleanStaging() error {
	rt.mustValid()
	staging := rt.stagingPath()

	entries, err := os.ReadDir(staging)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return &PathError{Op: "clean_staging", Path: staging, Err: err}
	}

	for _, entry := range entries {
		target := filepath.Join(staging, entry.Name())
		if err := safeRemoveAll(staging, target); err != nil {
			return err
		}
	}
	return nil
}

// RemoveStagingDir removes a staging directory by path. The path must
// be a direct child of <base>/.staging/. Returns nil if the directory
// does not exist.
func (rt BytecodeFS) RemoveStagingDir(path string) error {
	rt.mustValid()
	return safeRemoveAll(rt.stagingPath(), path)
}

// ProgramDirEntry represents a directory under <base>/programs/.
type ProgramDirEntry struct {
	Path     string // Full path to the directory
	KernelID uint32 // Parsed kernel ID; 0 if name is not numeric
	Numeric  bool   // True if directory name is a valid uint32
}

// ScanProgramDirs returns all directories under <base>/programs/.
// Returns nil (not error) if the programs directory does not exist.
func (rt BytecodeFS) ScanProgramDirs() ([]ProgramDirEntry, error) {
	rt.mustValid()
	programsPath := rt.programsPath()

	entries, err := os.ReadDir(programsPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, &PathError{Op: "scan_programs", Path: programsPath, Err: err}
	}

	var result []ProgramDirEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		pde := ProgramDirEntry{
			Path: filepath.Join(programsPath, name),
		}
		// Try to parse as uint32
		var id uint32
		if n, _ := fmt.Sscanf(name, "%d", &id); n == 1 {
			pde.KernelID = id
			pde.Numeric = true
		}
		result = append(result, pde)
	}
	return result, nil
}

// ScanStagingDirs returns all entry paths under <base>/.staging/.
// Returns nil (not error) if the staging directory does not exist.
func (rt BytecodeFS) ScanStagingDirs() ([]string, error) {
	rt.mustValid()
	stagingPath := rt.stagingPath()

	entries, err := os.ReadDir(stagingPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, &PathError{Op: "scan_staging", Path: stagingPath, Err: err}
	}

	var result []string
	for _, entry := range entries {
		result = append(result, filepath.Join(stagingPath, entry.Name()))
	}
	return result, nil
}
