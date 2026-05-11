// fire is the e2e built-in for spawning deterministic
// kernel-stimulus workers. It is a typed wrapper over start: the
// script states the kind of stimulus (unlinkat / kill / uprobe)
// and the wave-protocol parameters; fire owns binary resolution,
// env-var construction, and process shaping. See
// docs/PLAN-fire-builtin.md for the design rationale.
//
// fire is for syscall / signal / uprobe event generators only.
// A richer fixture surface, if needed, lives in its own subsystem,
// not in this registry.
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/frobware/go-bpfman/shell"
)

// fireKind describes one fire kind: the BPFMAN_SHELL_MODE value
// passed to the spawned bpfman-shell, a short summary for help
// and completion text, and a NeedsBinary flag that controls
// whether the resulting Job carries a target_binary path the
// script can read via $work.target_binary.
//
// NeedsBinary == true: the kind's correctness depends on the
// kernel attachment surface (uprobe targets a symbol in this
// binary), so target_binary is the running bpfman-shell ELF and
// carries the semantic guarantee. NeedsBinary == false: the
// kind's effect is purely a syscall or signal; target_binary is
// not exposed on the Job, and a script that tries to read it
// receives a runtime field error rather than a silent empty
// string.
type fireKind struct {
	Mode        string
	Summary     string
	NeedsBinary bool
}

// fireKinds is populated from the helper files' init functions.
// Each helper that implements a BPFMAN_SHELL_MODE registers its
// script-facing name and the kind metadata here. The
// BPFMAN_SHELL_MODE switch in main.go:runMode remains the
// authoritative dispatch; this registry is the script-facing
// index.
var fireKinds = map[string]fireKind{}

// registerFireKind adds a kind to the registry. Called from each
// helper file's init.
func registerFireKind(name string, k fireKind) {
	fireKinds[name] = k
}

// handleFire is the builtin handler for `fire KIND SENTINEL ACK
// --count N [--waves K]`. It resolves the kind from the registry,
// composes the BPFMAN_SHELL_MODE env, and delegates to spawnJob
// with /proc/self/exe as the executable. `start env
// BPFMAN_SHELL_MODE=...` remains valid as a debug escape hatch
// because fire is sugar over start, not a replacement.
//
// --count is required (per-wave fire count). --waves defaults to
// 1. Both must be non-negative integers; --waves must be >= 1.
// An unknown kind is rejected at runtime with a list of the
// registered names; v2 will hoist this check into the parser.
//
// Both spellings are accepted for each flag: `--count N` (space
// separated) and `--count=N` (equals form). The space form is
// what the scripts use because the equals form's interpolation
// of a variable (`--count=$n`) tokenises into two args
// (`--count=` plus the value) under the shell's word-splitting
// rules; quoting the whole flag as `"--count=${n}"` keeps the
// equals form intact but adds noise at every call site, so the
// space form is the primary surface. The equals form is still
// recognised so a literal value (`--count=5`) and the kill-style
// `--key=value` spelling work without surprise.
func handleFire(c builtinCtx) (shell.Value, error) {
	var positional []string
	count := ""
	waves := "1"
	for i := 0; i < len(c.Args); {
		text := argText(c.Args[i])
		switch {
		case text == "--count":
			if i+1 >= len(c.Args) {
				return shell.Value{}, fmt.Errorf("fire: --count requires a value")
			}
			count = argText(c.Args[i+1])
			i += 2
		case strings.HasPrefix(text, "--count="):
			count = strings.TrimPrefix(text, "--count=")
			i++
		case text == "--waves":
			if i+1 >= len(c.Args) {
				return shell.Value{}, fmt.Errorf("fire: --waves requires a value")
			}
			waves = argText(c.Args[i+1])
			i += 2
		case strings.HasPrefix(text, "--waves="):
			waves = strings.TrimPrefix(text, "--waves=")
			i++
		case strings.HasPrefix(text, "--"):
			return shell.Value{}, fmt.Errorf("fire: unknown flag %q", text)
		default:
			positional = append(positional, text)
			i++
		}
	}
	if len(positional) != 3 {
		return shell.Value{}, fmt.Errorf("fire: expected 3 positional arguments (KIND SENTINEL ACK), got %d", len(positional))
	}
	kindName, sentinel, ack := positional[0], positional[1], positional[2]
	kind, ok := fireKinds[kindName]
	if !ok {
		return shell.Value{}, fmt.Errorf("fire: unknown kind %q (registered: %s)", kindName, strings.Join(fireKindNames(), ", "))
	}
	if count == "" {
		return shell.Value{}, fmt.Errorf("fire %s: --count is required", kindName)
	}
	if n, err := strconv.Atoi(count); err != nil {
		return shell.Value{}, fmt.Errorf("fire %s: --count: %w", kindName, err)
	} else if n < 0 {
		return shell.Value{}, fmt.Errorf("fire %s: --count must not be negative (got %d)", kindName, n)
	}
	if k, err := strconv.Atoi(waves); err != nil {
		return shell.Value{}, fmt.Errorf("fire %s: --waves: %w", kindName, err)
	} else if k < 1 {
		return shell.Value{}, fmt.Errorf("fire %s: --waves must be at least 1 (got %d)", kindName, k)
	}
	shellPath, err := os.Executable()
	if err != nil {
		return shell.Value{}, fmt.Errorf("fire %s: resolve executable path: %w", kindName, err)
	}

	env := append(os.Environ(), "BPFMAN_SHELL_MODE="+kind.Mode)
	argv := []string{shellPath, sentinel, ack, count, waves}
	var targetBinary string
	if kind.NeedsBinary {
		targetBinary = shellPath
	}
	job, err := spawnJob(c.Ctx, c.Env, spawnSpec{
		Argv:         argv,
		Env:          env,
		Origin:       c.Pos.cite(),
		TargetBinary: targetBinary,
	})
	if err != nil {
		return shell.Value{}, fmt.Errorf("fire %s: %w", kindName, err)
	}
	return shell.ValueFromJob(job), nil
}

// fireKindNames returns the registered kind names sorted for
// stable diagnostic rendering.
func fireKindNames() []string {
	names := make([]string, 0, len(fireKinds))
	for name := range fireKinds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
