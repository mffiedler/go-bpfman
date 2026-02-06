package fhs

import (
	"fmt"
	"os"
	"path/filepath"
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

// Runtime provides regular-filesystem operations for bytecode
// persistence. Fields are unexported; obtain via Root.Runtime().
type Runtime struct {
	root Root
}

// valid reports whether the Runtime was obtained from a valid Root.
func (rt Runtime) valid() bool {
	return rt.root.valid()
}

// Valid reports whether the Runtime was obtained from a valid Root.
func (rt Runtime) Valid() bool {
	return rt.valid()
}

// programsPath returns <base>/programs.
func (rt Runtime) programsPath() string {
	return filepath.Join(rt.root.base, programsDir)
}

// stagingPath returns <base>/.staging.
func (rt Runtime) stagingPath() string {
	return filepath.Join(rt.root.base, stagingDir)
}

// programDir returns <base>/programs/{id}.
func (rt Runtime) programDir(id uint32) string {
	return filepath.Join(rt.root.base, programsDir, uitoa(id))
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
func (rt Runtime) PublishBytecode(id uint32, srcPath string, prov Provenance) error {
	if !rt.valid() {
		return ErrInvalidRoot
	}

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
			os.RemoveAll(tmpDir)
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
func (rt Runtime) RemoveProgram(id uint32) error {
	if !rt.valid() {
		return ErrInvalidRoot
	}
	return safeRemoveAll(rt.programsPath(), rt.programDir(id))
}

// ProgramExists reports whether <base>/programs/{id}/ exists.
func (rt Runtime) ProgramExists(id uint32) bool {
	if !rt.valid() {
		return false
	}
	var exists bool
	_ = (statExistsOp{path: rt.programDir(id), exists: &exists}).exec()
	return exists
}

// ProgramBytecodePath returns the published bytecode path for DB
// ObjectPath storage.
func (rt Runtime) ProgramBytecodePath(id uint32) string {
	return filepath.Join(rt.programDir(id), bytecodeName)
}

// CleanStaging removes all entries under <base>/.staging/. Staging is
// a writer-only concern and is never visible to readers.
func (rt Runtime) CleanStaging() error {
	if !rt.valid() {
		return ErrInvalidRoot
	}
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
