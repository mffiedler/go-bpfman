package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/server"
)

// ServeCmd starts the gRPC daemon.
type ServeCmd struct {
	TCPAddress   string `name:"tcp-address" help:"TCP address for gRPC server." default:"[::]:50051"`
	CSISupport   bool   `name:"csi-support" help:"Enable CSI driver support."`
	PprofAddress string `name:"pprof-address" help:"Address for pprof HTTP server. Port 0 selects an ephemeral port. Empty string disables." env:"BPFMAN_PPROF_ADDRESS" default:"localhost:0"`
}

// Run executes the serve command.
func (c *ServeCmd) Run(cli *CLI, ctx context.Context) error {
	logger, err := cli.LoggerFromConfig()
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}

	appConfig, err := cli.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	root, err := cli.Root()
	if err != nil {
		return fmt.Errorf("invalid runtime directory: %w", err)
	}

	cfg := server.RunConfig{
		Root:         root,
		TCPAddress:   c.TCPAddress,
		CSISupport:   c.CSISupport,
		PprofAddress: c.PprofAddress,
		Logger:       logger,
		Config:       appConfig,
	}

	return server.Run(ctx, cfg)
}
