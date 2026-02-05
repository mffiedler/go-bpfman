package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/image/oci"
	"github.com/frobware/go-bpfman/interpreter/image/verify"
	"github.com/frobware/go-bpfman/manager"
)

// CLIRuntime provides manager access for CLI commands.
// It wraps the RuntimeEnv and image puller, providing a unified interface
// for CLI commands to interact with the BPF manager directly.
type CLIRuntime struct {
	Manager *manager.Manager
	Puller  interpreter.ImagePuller
	Store   interpreter.Store
	Kernel  interpreter.KernelOperations
	Root    fs.Root
	Logger  *slog.Logger
	env     *manager.RuntimeEnv
}

// NewCLIRuntime creates a runtime environment for CLI commands.
// This sets up the manager, store, kernel adapter, and image puller
// based on the CLI configuration. The returned runtime must be closed
// when no longer needed.
func (c *CLI) NewCLIRuntime(ctx context.Context) (*CLIRuntime, error) {
	logger, err := c.Logger()
	if err != nil {
		return nil, fmt.Errorf("create logger: %w", err)
	}

	root, err := c.Root()
	if err != nil {
		return nil, fmt.Errorf("invalid runtime directory: %w", err)
	}

	// Set up runtime environment (ensures directories, opens store, creates manager)
	env, err := manager.SetupRuntimeEnv(ctx, root, logger)
	if err != nil {
		return nil, fmt.Errorf("setup runtime: %w", err)
	}

	// Load config for signature verification settings
	cfg, err := c.LoadConfig()
	if err != nil {
		if closeErr := env.Close(); closeErr != nil {
			logger.Warn("failed to close runtime env during cleanup", "error", closeErr)
		}
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Build signature verifier based on config
	var verifier interpreter.SignatureVerifier
	if cfg.Signing.ShouldVerify() {
		logger.Info("signature verification enabled")
		verifier = verify.Cosign(
			verify.WithLogger(logger),
			verify.WithAllowUnsigned(cfg.Signing.AllowUnsigned),
		)
	} else {
		logger.Debug("signature verification disabled")
		verifier = verify.NoSign()
	}

	// Create image puller for OCI images
	puller, err := oci.NewPuller(
		oci.WithLogger(logger),
		oci.WithVerifier(verifier),
	)
	if err != nil {
		if closeErr := env.Close(); closeErr != nil {
			logger.Warn("failed to close runtime env during cleanup", "error", closeErr)
		}
		return nil, fmt.Errorf("create image puller: %w", err)
	}

	return &CLIRuntime{
		Manager: env.Manager,
		Puller:  puller,
		Store:   env.Store,
		Kernel:  env.Kernel,
		Root:    root,
		Logger:  logger,
		env:     env,
	}, nil
}

// Close releases resources held by the CLI runtime.
func (r *CLIRuntime) Close() error {
	if r.env != nil {
		return r.env.Close()
	}
	return nil
}
