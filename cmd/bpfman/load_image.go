package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

// LoadImageCmd loads BPF programs from an OCI container image.
type LoadImageCmd struct {
	OutputFlags
	MetadataFlags
	GlobalDataFlags

	Example      ExampleFlag      `name:"example" help:"Show working examples and exit."`
	ImageURL     string           `short:"i" name:"image-url" help:"OCI image reference (e.g., quay.io/bpfman-bytecode/xdp_pass:latest)." required:""`
	Programs     []ProgramSpec    `name:"programs" help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (can be repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the image are loaded."`
	PullPolicy   ImagePullPolicy  `short:"p" name:"pull-policy" help:"Image pull policy (Always, IfNotPresent, Never)." default:"IfNotPresent"`
	RegistryAuth string           `name:"registry-auth" env:"BPFMAN_REGISTRY_AUTH" help:"Base64-encoded registry auth (username:password). Prefer BPFMAN_REGISTRY_AUTH env var to avoid exposing credentials in process listings."`
	Application  string           `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`
	MapOwnerID   kernel.ProgramID `name:"map-owner-id" help:"Program ID of another program to share maps with."`
}

// Run executes the load image command.
func (c *LoadImageCmd) Run(cli *CLI, ctx context.Context) error {
	// Parse pull policy (before acquiring lock)
	pullPolicy, ok := bpfman.ParseImagePullPolicy(c.PullPolicy.Value)
	if !ok {
		return fmt.Errorf("invalid pull policy %q", c.PullPolicy.Value)
	}

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
		"pull_policy", c.PullPolicy.Value,
	)

	// loadImageResult captures the result of a load image operation.
	type loadImageResult struct {
		Programs []bpfman.Program
	}

	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (loadImageResult, error) {
		var res loadImageResult

		// Parse auth config from base64-encoded registry-auth
		var auth *platform.ImageAuth
		if c.RegistryAuth != "" {
			username, password, parseErr := parseRegistryAuth(c.RegistryAuth)
			if parseErr != nil {
				return res, fmt.Errorf("invalid registry-auth: %w", parseErr)
			}
			logger.Debug("using registry auth", "username", username)
			auth = &platform.ImageAuth{
				Username: username,
				Password: password,
			}
		}

		// Convert global data
		var globalData map[string][]byte
		if len(c.GlobalData) > 0 {
			globalData = GlobalDataMap(c.GlobalData)
		}

		// Build metadata map, adding application if specified
		metadata := MetadataMap(c.Metadata)
		if c.Application != "" {
			if metadata == nil {
				metadata = make(map[string]string)
			}
			metadata["bpfman.io/application"] = c.Application
		}

		// Build image ref
		ref := platform.ImageRef{
			URL:        c.ImageURL,
			PullPolicy: pullPolicy,
			Auth:       auth,
		}

		// Convert CLI ProgramSpec to manager.ProgramSpec
		var programs []manager.ProgramSpec
		for _, prog := range c.Programs {
			programs = append(programs, manager.ProgramSpec{
				Name:       prog.Name,
				Type:       prog.Type,
				AttachFunc: prog.AttachFunc,
				MapOwnerID: c.MapOwnerID,
			})
		}

		loaded, loadErr := mgr.Load(ctx, manager.LoadSource{
			Image: &ref,
		}, programs, manager.LoadOpts{
			UserMetadata: metadata,
			GlobalData:   globalData,
		})

		if loadErr != nil {
			return loadImageResult{}, fmt.Errorf("failed to load from image: %w", loadErr)
		}
		return loadImageResult{Programs: loaded}, nil
	})
	if err != nil {
		return err
	}

	// Format and emit output outside the lock
	output, err := FormatLoadedPrograms(result.Programs, &c.OutputFlags)
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
