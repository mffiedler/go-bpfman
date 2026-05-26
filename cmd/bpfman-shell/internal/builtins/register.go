package builtins

import (
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
)

// Register installs one builtin in the shared shell registry.
// Builtins that live in this package call it from init() so the
// handler and help text stay colocated.
func Register(b driver.Builtin) {
	driver.RegisterBuiltin(b)
}
