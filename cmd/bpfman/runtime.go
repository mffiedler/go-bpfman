package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/bpfmanfs/runtime"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/ebpf"
	"github.com/frobware/go-bpfman/interpreter/image/oci"
	"github.com/frobware/go-bpfman/interpreter/image/verify"
	"github.com/frobware/go-bpfman/interpreter/store/sqlite"
	"github.com/frobware/go-bpfman/manager"
)

// NewManager creates a manager for CLI commands.
// Returns the manager and a cleanup function that releases resources.
// The cleanup function should be called when the manager is no longer needed.
func (c *CLI) NewManager(ctx context.Context) (*manager.Manager, func() error, error) {
	layout, err := c.Layout()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid runtime directory: %w", err)
	}

	logger := c.Logger()

	// Create store
	store, err := sqlite.New(ctx, layout.DBPath(), logger)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}

	// Create kernel adapter
	kernel := ebpf.New(ebpf.WithLogger(logger))

	// Ensure runtime directories and bpffs mount
	ensuredRuntime, err := runtime.New(layout, runtime.RealMounter{}, logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("ensure runtime: %w", err)
	}

	// Create manager (no image puller for file-based CLI operations)
	mgr, err := manager.New(ensuredRuntime, nil, store, kernel, ebpf.NewProgramDiscoverer(), logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("create manager: %w", err)
	}

	cleanup := func() error {
		return store.Close()
	}

	return mgr, cleanup, nil
}

// NewManagerWithPuller creates a manager with an image puller for CLI commands
// that need to load from OCI images. Returns the manager, a cleanup function,
// and the image puller.
func (c *CLI) NewManagerWithPuller(ctx context.Context) (*manager.Manager, func() error, error) {
	layout, err := c.Layout()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid runtime directory: %w", err)
	}

	logger := c.Logger()

	// Create store
	store, err := sqlite.New(ctx, layout.DBPath(), logger)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}

	// Create kernel adapter
	kernel := ebpf.New(ebpf.WithLogger(logger))

	// Ensure runtime directories and bpffs mount
	ensuredRuntime, err := runtime.New(layout, runtime.RealMounter{}, logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("ensure runtime: %w", err)
	}

	// Build image puller with signature verification settings from config
	puller, err := c.buildImagePuller(logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("create image puller: %w", err)
	}

	// Create manager with image puller
	mgr, err := manager.New(ensuredRuntime, puller, store, kernel, ebpf.NewProgramDiscoverer(), logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("create manager: %w", err)
	}

	cleanup := func() error {
		return store.Close()
	}

	return mgr, cleanup, nil
}

// buildImagePuller creates an image puller with signature verification settings from config.
func (c *CLI) buildImagePuller(logger interface{ Debug(string, ...any) }) (interpreter.ImagePuller, error) {
	cfg, err := c.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	cache, err := c.EnsuredImageCache()
	if err != nil {
		return nil, fmt.Errorf("ensure image cache: %w", err)
	}

	// Build signature verifier based on config
	var verifier interpreter.SignatureVerifier
	slogLogger := c.Logger()
	if cfg.Signing.ShouldVerify() {
		slogLogger.Info("signature verification enabled")
		verifier = verify.Cosign(
			verify.WithLogger(slogLogger),
			verify.WithAllowUnsigned(cfg.Signing.AllowUnsigned),
		)
	} else {
		slogLogger.Debug("signature verification disabled")
		verifier = verify.NoSign()
	}

	return oci.NewPuller(
		cache,
		oci.WithLogger(slogLogger),
		oci.WithVerifier(verifier),
	)
}
