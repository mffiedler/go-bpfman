package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/platform/image/verify"
)

// ImageCmd groups image-related subcommands.
type ImageCmd struct {
	Verify ImageVerifyCmd `cmd:"" help:"Verify an OCI image signature."`
}

// ImageVerifyCmd verifies the signature of an OCI image.
type ImageVerifyCmd struct {
	ImageURL string `arg:"" name:"image" help:"OCI image reference (e.g., quay.io/bpfman-bytecode/xdp_pass:latest)."`

	// Signing configuration
	AllowUnsigned *bool `name:"allow-unsigned" help:"Allow unsigned images (overrides config file)."`
}

// Run executes the image verify command.
func (c *ImageVerifyCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	logger := cli.Logger()
	logger.Info("verifying image signature", "image", c.ImageURL)

	// Load configuration (use CLI's config, not the deprecated --config flag)
	cfg, err := cli.LoadConfig()
	if err != nil {
		logger.Warn("failed to load config file, using defaults", "path", cli.Config, "error", err)
		cfg = config.DefaultConfig()
	}

	// Apply CLI overrides
	if c.AllowUnsigned != nil {
		cfg.Signing.AllowUnsigned = *c.AllowUnsigned
	}

	// For verify command, always enable verification (that's the point)
	cfg.Signing.VerifyEnabled = true

	logger.Debug("signing configuration",
		"allow_unsigned", cfg.Signing.AllowUnsigned,
	)

	// Create verifier
	verifier := verify.Cosign(
		verify.WithLogger(logger),
		verify.WithAllowUnsigned(cfg.Signing.AllowUnsigned),
	)

	// Verify
	if err := verifier.Verify(ctx, c.ImageURL); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	return cli.PrintOutf("Image %s: signature verified\n", c.ImageURL)
}
