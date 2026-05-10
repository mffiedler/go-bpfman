// bpfman-shell is the development / test / ops companion to bpfman.
// It hosts the REPL, the DSL script runner, and (in time) the test
// scaffolding subcommands (veth, reap, lease). Production deployments
// ship only bin/bpfman; bin/bpfman-shell is for dev and CI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// main wires the process-level signal model. The root context
// is plain context.Background(): we deliberately do not put
// SIGINT/SIGTERM on the root because the two execution modes
// want different things from those signals. Script mode wraps
// the root with NotifyContext inside replScript so a ^C aborts
// the whole script (matches running a bash script). Interactive
// mode wraps each chunk in its own NotifyContext inside
// replInteractive so a ^C cancels the current builtin or
// foreground operation but the shell stays alive at the
// prompt; this is the bash-shaped REPL contract.
//
// A small watcher goroutine catches a second SIGINT/SIGTERM
// after the first has been observed by the running mode and
// hard-exits, so a wedged shell can always be killed by typing
// ^C twice. The first signal goes to the mode's NotifyContext;
// the second is the escape hatch.
func main() {
	// Mode dispatch: when BPFMAN_SHELL_MODE is set, bpfman-shell
	// acts as a test-fixture helper rather than a REPL/script
	// runner. The dispatch runs before NewCLI so we don't open
	// the manager / database / lock for a helper invocation that
	// has no need for them. Mirrors the BPFMAN_MODE pattern used
	// by the main bpfman binary for bpfman-rpc / bpfman-ns.
	if mode := os.Getenv("BPFMAN_SHELL_MODE"); mode != "" {
		if err := runMode(mode, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "bpfman-shell: %v\n", err)
			os.Exit(1)
		}
		return
	}

	c, err := NewCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bpfman-shell: error: %v\n", err)
		os.Exit(1)
	}

	go watchForHardExit()

	if err := c.Execute(context.Background()); err != nil {
		os.Exit(1)
	}
}

// runMode dispatches the BPFMAN_SHELL_MODE entry points. Each mode
// is a test-fixture helper that runs in place of the REPL/script
// runner; modes are intentionally narrow, single-purpose, and
// self-contained so the helper code does not pull the rest of the
// shell's machinery in.
func runMode(mode string, args []string) error {
	switch mode {
	case "uprobe-fire-worker":
		return runUprobeFireWorker(args)
	default:
		return fmt.Errorf("unknown BPFMAN_SHELL_MODE %q", mode)
	}
}

// watchForHardExit installs a long-lived signal watcher that
// hard-exits the process on the second SIGINT or SIGTERM. The
// first signal is consumed by whatever mode-specific
// NotifyContext is active (script-wide or per-chunk); if that
// mode's context cancellation is observed and acted on, the
// shell keeps running. A user who sends a second signal has
// asked unambiguously for the process to die, and we honour
// that without further negotiation.
func watchForHardExit() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig // first signal observed
	<-sig // second signal: hard exit
	os.Exit(1)
}
