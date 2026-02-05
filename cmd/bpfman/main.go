// bpfman is a minimal BPF program manager.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "bpfman: error: must run as root")
		os.Exit(1)
	}

	c, kctx, err := New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bpfman: error: %v\n", err)
		os.Exit(1)
	}
	if c == nil {
		// Namespace helper handled the request
		return
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

	if err := c.Execute(ctx, kctx); err != nil {
		os.Exit(1)
	}
}
