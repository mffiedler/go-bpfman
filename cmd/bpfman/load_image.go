package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

// LoadImageCmd loads BPF programs from an OCI container image.
type LoadImageCmd struct {
	cliformat.OutputFlags
	MetadataFlags
	GlobalDataFlags

	Example      ExampleFlag             `name:"example" help:"Show working examples and exit."`
	ImageURL     string                  `short:"i" name:"image-url" help:"OCI image reference (e.g., quay.io/bpfman-bytecode/xdp_pass:latest)." required:""`
	Programs     []bpfmancli.ProgramSpec `name:"programs" sep:"," help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (comma-separated or repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the image are loaded."`
	PullPolicy   bpfman.ImagePullPolicy  `short:"p" name:"pull-policy" help:"Image pull policy (Always, IfNotPresent, Never)." default:"IfNotPresent"`
	RegistryAuth string                  `name:"registry-auth" env:"BPFMAN_REGISTRY_AUTH" help:"Base64-encoded registry auth (username:password). Prefer BPFMAN_REGISTRY_AUTH env var to avoid exposing credentials in process listings."`
	Application  string                  `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`
	MapOwnerID   kernel.ProgramID        `name:"map-owner-id" help:"Program ID of another program to share maps with."`
}

// Run executes the load image command.
func (c *LoadImageCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	logger := cli.Logger()

	// Use NewManagerWithPuller for image loading operations
	mgr, cleanup, err := cli.NewManagerWithPuller(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	logger.Info("loading BPF programs from OCI image",
		"image", c.ImageURL,
		"programs", len(c.Programs),
		"pull_policy", c.PullPolicy.String(),
	)

	// loadImageResult captures the result of a load image operation.
	type loadImageResult struct {
		Programs []bpfman.Program
	}

	// load is lockless by construction (docs/PLAN-load-lockless.md):
	// the OCI pull, kernel BPF_PROG_LOAD, bytecode publish, and
	// single sqlite commit transaction all run without acquiring
	// the writer flock.

	// Parse auth config from base64-encoded registry-auth.
	var auth *platform.ImageAuth
	if c.RegistryAuth != "" {
		username, password, parseErr := parseRegistryAuth(c.RegistryAuth)
		if parseErr != nil {
			return fmt.Errorf("invalid registry-auth: %w", parseErr)
		}
		logger.Debug("using registry auth", "username", username)
		auth = &platform.ImageAuth{
			Username: username,
			Password: password,
		}
	}

	var globalData map[string][]byte
	if len(c.GlobalData) > 0 {
		globalData = bpfmancli.GlobalDataMap(c.GlobalData)
	}

	ref := platform.ImageRef{
		URL:        c.ImageURL,
		PullPolicy: c.PullPolicy,
		Auth:       auth,
	}

	req := manager.NewLoadRequest(manager.LoadSource{Image: &ref}, loadProgramSpecs(c.Programs), manager.LoadRequestOpts{
		UserMetadata: bpfmancli.MetadataMap(c.Metadata),
		GlobalData:   globalData,
		Application:  c.Application,
		MapOwnerID:   c.MapOwnerID,
	})
	loaded, err := mgr.LoadRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to load from image: %w", err)
	}
	result := loadImageResult{Programs: loaded}

	output, err := cliformat.FormatLoadedPrograms(result.Programs, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// parseRegistryAuth parses a base64-encoded "username:password" string.
func parseRegistryAuth(encoded string) (username, password string, err error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", fmt.Errorf("invalid base64 encoding: %w", err)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("expected 'username:password' format")
	}

	return parts[0], parts[1], nil
}
