package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/cmd/internal/runtime"
	"github.com/frobware/go-bpfman/server"
	"github.com/frobware/go-bpfman/version"
)

// ServeCmd starts the gRPC daemon.
type ServeCmd struct {
	TCPAddress   string `name:"tcp-address" help:"TCP address for gRPC server." default:"[::]:50051"`
	CSISupport   bool   `name:"csi-support" help:"Enable CSI driver support."`
	PprofAddress string `name:"pprof-address" help:"Address for pprof HTTP server. Port 0 selects an ephemeral port. Empty string disables." env:"BPFMAN_PPROF_ADDRESS" default:"localhost:0"`
	// SocketPath defaults to /run/bpfman-sock/bpfman.sock for compatibility
	// with bpfman-operator which expects the socket at this location.
	SocketPath string `name:"socket-path" help:"Unix socket path for gRPC server." env:"BPFMAN_SOCKET_PATH" default:"/run/bpfman-sock/bpfman.sock"`
}

// Run executes the serve command.
func (c *ServeCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	logger, err := cli.LoggerFromConfig()
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}

	logger.Info("starting bpfman", "version", version.Get().String())

	appConfig, err := cli.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	layout, err := cli.Layout()
	if err != nil {
		return fmt.Errorf("invalid runtime directory: %w", err)
	}

	imageCache, err := cli.EnsuredImageCache()
	if err != nil {
		return fmt.Errorf("ensure image cache directory: %w", err)
	}

	cfg := server.RunConfig{
		Layout:       layout,
		ImageCache:   imageCache,
		TCPAddress:   c.TCPAddress,
		CSISupport:   c.CSISupport,
		PprofAddress: c.PprofAddress,
		SocketPath:   c.SocketPath,
		Logger:       logger,
		Config:       appConfig,
	}

	return server.Run(ctx, cfg)
}
