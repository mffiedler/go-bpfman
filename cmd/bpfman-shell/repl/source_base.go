// Interactive-source base directory plumbing. The interactive
// REPL has no `cd` builtin, so cwd is fixed for the life of the
// loop. EnsureInteractiveBaseDir captures os.Getwd() once at loop
// entry and stores it on the context so subsequent `source FOO`
// resolutions anchor to a stable directory, not whatever value
// a parallel test that called os.Chdir later happens to leave
// in the global. WithInteractiveBaseDir is the test seam that
// injects a specific directory directly, avoiding os.Chdir in
// t.Parallel tests.

package repl

import (
	"context"
	"os"
)

type contextKey int

const interactiveBaseDirKey contextKey = iota

// EnsureInteractiveBaseDir attaches the directory that
// interactive `source FOO` should resolve relative paths against,
// if one is not already present on the context. Called once at
// loop entry. Failures of os.Getwd are swallowed: the only
// consumer (handleSource) falls back to leaving the path as-is,
// matching the pre-refactor behaviour where os.Open would simply
// have used whatever cwd it could see.
func EnsureInteractiveBaseDir(ctx context.Context) context.Context {
	if ctx.Value(interactiveBaseDirKey) != nil {
		return ctx
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, interactiveBaseDirKey, cwd)
}

// WithInteractiveBaseDir injects a base directory for
// interactive `source` resolution. Test seam: tests that want
// to assert interactive cwd-relative behaviour use this instead
// of os.Chdir, since os.Chdir mutates process-global state and
// races other t.Parallel tests reading the cwd.
func WithInteractiveBaseDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, interactiveBaseDirKey, dir)
}

// InteractiveBaseDir returns the captured base directory, or ""
// if none was attached (in which case the caller should fall
// through to the previous cwd-relative path).
func InteractiveBaseDir(ctx context.Context) string {
	if v, ok := ctx.Value(interactiveBaseDirKey).(string); ok {
		return v
	}
	return ""
}
