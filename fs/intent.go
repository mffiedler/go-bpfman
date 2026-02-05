package fs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Internal primitives: operations as data structures executed by
// osInterp. Public methods build sequences of these primitives;
// only osInterp calls os.*.

// op is the internal operation interface.
type op interface {
	exec() error
}

// mkdirAllOp creates a directory and all parents.
type mkdirAllOp struct {
	path string
	perm os.FileMode
}

func (o mkdirAllOp) exec() error {
	return os.MkdirAll(o.path, o.perm)
}

// mkdirTempOp creates a temporary directory under dir with the given
// pattern. The created path is stored in *result.
type mkdirTempOp struct {
	dir     string
	pattern string
	result  *string
}

func (o mkdirTempOp) exec() error {
	p, err := os.MkdirTemp(o.dir, o.pattern)
	if err != nil {
		return err
	}
	*o.result = p
	return nil
}

// renameOp atomically renames oldpath to newpath.
type renameOp struct {
	oldpath string
	newpath string
}

func (o renameOp) exec() error {
	return os.Rename(o.oldpath, o.newpath)
}

// removeAllOp removes a path and all its contents.
type removeAllOp struct {
	path string
}

func (o removeAllOp) exec() error {
	return os.RemoveAll(o.path)
}

// copyFileOp copies a regular file from src to dst.
type copyFileOp struct {
	src  string
	dst  string
	perm os.FileMode
}

func (o copyFileOp) exec() error {
	sf, err := os.Open(o.src)
	if err != nil {
		return err
	}
	defer sf.Close()

	df, err := os.OpenFile(o.dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, o.perm)
	if err != nil {
		return err
	}
	defer df.Close()

	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return df.Close()
}

// writeJSONOp writes v as indented JSON to path.
type writeJSONOp struct {
	path string
	perm os.FileMode
	v    any
}

func (o writeJSONOp) exec() error {
	data, err := json.MarshalIndent(o.v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(o.path, data, o.perm)
}

// statExistsOp checks whether a path exists. Stores the result in
// *exists.
type statExistsOp struct {
	path   string
	exists *bool
}

func (o statExistsOp) exec() error {
	_, err := os.Stat(o.path)
	if err == nil {
		*o.exists = true
		return nil
	}
	if os.IsNotExist(err) {
		*o.exists = false
		return nil
	}
	return err
}

// osInterp executes a sequence of ops. It is the only code that
// calls os.* in the fs package.
func osInterp(ops []op) error {
	for _, o := range ops {
		if err := o.exec(); err != nil {
			return err
		}
	}
	return nil
}

// safeRemoveAll removes target only if it is under parent. This
// prevents accidental deletion of paths outside the expected tree.
//
// Uses filepath.Rel to avoid prefix false positives (e.g.,
// /run/bpfman/programsX matching /run/bpfman/programs).
func safeRemoveAll(parent, target string) error {
	rel, err := filepath.Rel(parent, target)
	if err != nil {
		return ErrOutsideRoot{Parent: parent, Target: target}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrOutsideRoot{Parent: parent, Target: target}
	}
	return os.RemoveAll(target)
}

// validateRegularFile checks that path exists and is a regular file.
func validateRegularFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return &PathError{Op: "validate", Path: path, Err: err}
	}
	if !fi.Mode().IsRegular() {
		return &PathError{Op: "validate", Path: path, Err: fmt.Errorf("not a regular file")}
	}
	return nil
}
