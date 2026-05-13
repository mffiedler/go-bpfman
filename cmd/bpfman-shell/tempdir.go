// tempdir creates a private temporary directory and returns a
// structured value carrying the absolute path on its .path field.
// The motivating use case is e2e fixtures that need per-invocation
// sentinel/ack paths so parallel script instances cannot collide
// on shared `/tmp/foo.go`-style paths; the same shape covers any
// future test that needs a private scratch directory.
//
// Lifecycle is caller-driven: pair with `defer rm -rf $wd.path` for
// the canonical cleanup. tempdir does not register itself in any
// auto-cleanup mechanism, which keeps the builtin small and matches
// the existing `net veth-pair` / `fire` / `start` pattern of
// "returns a handle; caller defers the teardown".

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// handleTempdir is the registry handler for `tempdir PREFIX`.
// PREFIX names the directory's leading component so concurrent
// invocations can be told apart in `ls /tmp`; os.MkdirTemp appends
// a random suffix to guarantee uniqueness. The directory is the
// caller's to clean up.
func handleTempdir(c builtinCtx) (shell.Value, error) {
	if len(c.Args) != 1 {
		return shell.Value{}, fmt.Errorf("tempdir: requires exactly one PREFIX argument")
	}
	prefix := strings.TrimSpace(argText(c.Args[0]))
	if prefix == "" {
		return shell.Value{}, fmt.Errorf("tempdir: PREFIX must not be empty")
	}
	dir, err := os.MkdirTemp("", prefix+".*")
	if err != nil {
		return shell.Value{}, fmt.Errorf("tempdir: %w", err)
	}
	return shell.ValueFromMap(map[string]any{"path": dir}), nil
}
