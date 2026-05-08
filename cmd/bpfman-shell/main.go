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

func main() {
	c, err := NewCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bpfman-shell: error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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
