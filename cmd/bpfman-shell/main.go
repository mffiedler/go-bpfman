// bpfman-shell is the development / test / ops companion to bpfman.
// It hosts the DSL script runner, inspection modes, and (in time) the
// test scaffolding subcommands (veth, reap, lease). Production
// deployments ship only bin/bpfman; bin/bpfman-shell is for dev and CI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/fixturemode"
)

// main wires the process-level signal model. The root context
// is plain context.Background(): the shell runner installs its
// own NotifyContext so a ^C aborts the whole program, matching
// the way a bash script exits on SIGINT.
//
// A small watcher goroutine catches a second SIGINT/SIGTERM
// after the first has been observed by the running program and
// hard-exits, so a wedged script can always be killed by typing
// ^C twice. The first signal goes to the runner's NotifyContext;
// the second is the escape hatch.
func main() {
	// Mode dispatch: when BPFMAN_SHELL_MODE is set, bpfman-shell
	// acts as a test-fixture helper rather than a user-facing
	// script entry point. The dispatch runs before NewCLI so we don't open
	// the manager / database / lock for a helper invocation that
	// has no need for them. Mirrors the BPFMAN_MODE pattern used
	// by the main bpfman binary for bpfman-rpc / bpfman-ns.
	if mode := os.Getenv("BPFMAN_SHELL_MODE"); mode != "" {
		if err := fixturemode.Run(mode, os.Args[1:]); err != nil {
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

// watchForHardExit installs a long-lived signal watcher that
// hard-exits the process on the second SIGINT or SIGTERM. The
// first signal is consumed by the runner's NotifyContext; if
// that context cancellation is observed and acted on, the
// process exits cleanly. A user who sends a second signal has
// asked unambiguously for the process to die, and we honour
// that without further negotiation.
func watchForHardExit() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig // first signal observed
	<-sig // second signal: hard exit
	os.Exit(1)
}
