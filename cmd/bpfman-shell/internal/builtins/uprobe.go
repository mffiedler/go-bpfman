// uprobe is the e2e built-in for deterministic userspace-probe
// targets owned by the running bpfman-shell process.
package builtins

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/fixturemode"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
)

func init() {
	Register(driver.Builtin{
		Name:     "uprobe",
		Handler:  handleUprobe,
		Category: driver.CategoryJobs,
		Usage:    "uprobe target  |  uprobe fire N",
		Summary:  "Resolve and fire the bpfman-shell uprobe fixture target.",
		Detail: "uprobe target returns the running bpfman-shell ELF path, " +
			"fixture symbol, and current PID. Attach uprobe/uretprobe " +
			"programs to that path and symbol, pass pid as expected_pid, " +
			"then use uprobe fire N to synchronously call the target N times.",
	})
}

func handleUprobe(c driver.Ctx) (runtime.Value, error) {
	if len(c.Args) == 0 {
		return runtime.Value{}, fmt.Errorf("uprobe: subcommand required (valid: target, fire)")
	}
	sub := driver.ArgText(c.Args[0])
	rest := c.Args[1:]
	switch sub {
	case "target":
		return handleUprobeTarget(rest)
	case "fire":
		return handleUprobeFire(rest)
	default:
		return runtime.Value{}, fmt.Errorf("uprobe: unknown subcommand %q (valid: target, fire)", sub)
	}
}

func handleUprobeTarget(args []runtime.Arg) (runtime.Value, error) {
	if len(args) != 0 {
		return runtime.Value{}, fmt.Errorf("uprobe target: takes no arguments")
	}
	exe, err := os.Executable()
	if err != nil {
		return runtime.Value{}, fmt.Errorf("uprobe target: resolve executable path: %w", err)
	}
	return runtime.ValueFromMap(map[string]any{
		"path":      exe,
		"pid":       json.Number(strconv.Itoa(os.Getpid())),
		"symbol":    fixturemode.UprobeTargetSymbol,
		"go_symbol": fixturemode.UprobeGoTargetSymbol,
	}), nil
}

func handleUprobeFire(args []runtime.Arg) (runtime.Value, error) {
	if len(args) != 1 {
		return runtime.Value{}, fmt.Errorf("uprobe fire: requires N")
	}
	n, err := strconv.Atoi(driver.ArgText(args[0]))
	if err != nil {
		return runtime.Value{}, fmt.Errorf("uprobe fire: N: %w", err)
	}
	if n < 0 {
		return runtime.Value{}, fmt.Errorf("uprobe fire: N must not be negative (got %d)", n)
	}
	fixturemode.FireUprobeTarget(n)
	return runtime.ValueFromEnvelope(runtime.OkEnvelope()), nil
}
