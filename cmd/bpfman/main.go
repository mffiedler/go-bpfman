// bpfman is a minimal BPF program manager.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/frobware/go-bpfman/ns/nsenter"
)

// NamespaceSwitcherResult represents the outcome of attempting to run
// as the namespace helper subprocess.
type NamespaceSwitcherResult struct {
	Ran bool
	Err error
}

// RunNamespaceSwitcher checks if we're being invoked as the namespace helper
// subprocess (for container uprobe attachment) and runs that code path if so.
func RunNamespaceSwitcher() NamespaceSwitcherResult {
	modeEnv := os.Getenv(nsenter.ModeEnvVar)
	inv, isHelper, err := DetectNamespaceHelperInvocation(os.Args, modeEnv)
	if err != nil {
		return NamespaceSwitcherResult{Err: err}
	}
	if !isHelper {
		return NamespaceSwitcherResult{}
	}
	return NamespaceSwitcherResult{Ran: true, Err: runNamespaceHelper(inv)}
}

// rootExempt reports whether the argument vector indicates an
// invocation that does not need root: "version" as a positional
// (the version subcommand) or "help" / --help / -h (usage output).
// Neither touches kernel, store, or filesystem state.
func rootExempt(args []string) bool {
	for _, a := range args {
		switch a {
		case "version", "help", "--help", "-h":
			return true
		}
	}
	return false
}

func main() {
	// Allow version and help to run without root. Neither touches
	// kernel, store, or filesystem state.
	if os.Geteuid() != 0 && !rootExempt(os.Args[1:]) {
		fmt.Fprintln(os.Stderr, "bpfman: error: must run as root")
		os.Exit(1)
	}

	// Check if we're being invoked as the namespace helper subprocess.
	// This is a completely different execution path with its own CLI.
	switch r := RunNamespaceSwitcher(); {
	case r.Ran && r.Err != nil:
		fmt.Fprintf(os.Stderr, "bpfman-ns: error: %v\n", r.Err)
		os.Exit(1)
	case r.Err != nil:
		fmt.Fprintf(os.Stderr, "bpfman: error: %v\n", r.Err)
		os.Exit(1)
	case r.Ran:
		return
	}

	c, err := NewCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bpfman: error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Second signal forces immediate exit. The first signal cancels ctx
	// for graceful shutdown; if the user sends another signal during
	// shutdown, exit immediately rather than waiting.
	go func() {
		<-ctx.Done()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		os.Exit(1)
	}()

	if err := c.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
