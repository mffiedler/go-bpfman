// bpfman is a minimal BPF program manager.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/frobware/go-bpfman/cmd/bpfman/cli"
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "bpfman: error: must run as root")
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

	cli.Run(ctx)
}
