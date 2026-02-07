package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/outcome"
)

// LoadImageCmd loads BPF programs from an OCI container image.
type LoadImageCmd struct {
	OutputFlags
	MetadataFlags
	GlobalDataFlags

	ImageURL     string          `short:"i" name:"image-url" help:"OCI image reference (e.g., quay.io/bpfman-bytecode/xdp_pass:latest)." required:""`
	Programs     []ProgramSpec   `name:"programs" help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (can be repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the image are loaded."`
	PullPolicy   ImagePullPolicy `short:"p" name:"pull-policy" help:"Image pull policy (Always, IfNotPresent, Never)." default:"IfNotPresent"`
	RegistryAuth string          `name:"registry-auth" env:"BPFMAN_REGISTRY_AUTH" help:"Base64-encoded registry auth (username:password). Prefer BPFMAN_REGISTRY_AUTH env var to avoid exposing credentials in process listings."`
	Application  string          `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`
	MapOwnerID   uint32          `name:"map-owner-id" help:"Program ID of another program to share maps with."`
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

	// loadImageResult captures both successful programs and any failure outcome.
	type loadImageResult struct {
		Programs      []bpfman.Program
		FailedOutcome outcome.OperationOutcome
	}

	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (loadImageResult, error) {
		var res loadImageResult

		// Parse auth config from base64-encoded registry-auth
		var username, password string
		if c.RegistryAuth != "" {
			var parseErr error
			username, password, parseErr = parseRegistryAuth(c.RegistryAuth)
			if parseErr != nil {
				return res, fmt.Errorf("invalid registry-auth: %w", parseErr)
			}
			logger.Debug("using registry auth", "username", username)
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

		// If no programs specified, use LoadImage for auto-discovery
		if len(c.Programs) == 0 {
			// Fall back to LoadImage for auto-discovery (legacy path)
			ref := interpreter.ImageRef{
				URL:        c.ImageURL,
				PullPolicy: pullPolicy,
			}
			if username != "" {
				ref.Auth = &interpreter.ImageAuth{
					Username: username,
					Password: password,
				}
			}
			loaded, err := mgr.LoadImage(ctx, mgr.ImagePuller(), ref, nil, manager.LoadImageOpts{
				UserMetadata: metadata,
				GlobalData:   globalData,
			})
			if err != nil {
				var me *manager.ManagerError
				if errors.As(err, &me) {
					res.FailedOutcome = me.Outcome
				}
				return res, fmt.Errorf("failed to load from image: %w", err)
			}
			res.Programs = loaded
			return res, nil
		}

		// Load each specified program using the unified Load interface
		var loaded []bpfman.Program
		for _, prog := range c.Programs {
			// Build LoadSpec for this program from the image
			var spec bpfman.LoadSpec
			var specErr error
			if prog.Type.RequiresAttachFunc() {
				spec, specErr = bpfman.NewImageAttachLoadSpec(c.ImageURL, prog.Name, prog.Type, prog.AttachFunc, pullPolicy)
			} else {
				spec, specErr = bpfman.NewImageLoadSpec(c.ImageURL, prog.Name, prog.Type, pullPolicy)
			}
			if specErr != nil {
				return res, fmt.Errorf("invalid load spec for %q: %w", prog.Name, specErr)
			}

			// Add auth if configured
			if username != "" {
				spec = spec.WithImageAuth(username, password)
			}

			// Add global data if configured
			if globalData != nil {
				spec = spec.WithGlobalData(globalData)
			}

			// Set map owner ID if specified
			if c.MapOwnerID != 0 {
				spec = spec.WithMapOwnerID(c.MapOwnerID)
			}

			// Load through manager using unified interface
			loadedProg, loadErr := mgr.Load(ctx, spec, manager.LoadOpts{
				UserMetadata: metadata,
			})
			if loadErr != nil {
				var me *manager.ManagerError
				if errors.As(loadErr, &me) {
					res.FailedOutcome = me.Outcome
				}
				return res, fmt.Errorf("failed to load program %q from image: %w", prog.Name, loadErr)
			}

			logger.Info("program loaded successfully",
				"name", loadedProg.Record.Meta.Name,
				"kernel_id", loadedProg.Record.KernelID,
				"pin_path", loadedProg.Record.Handles.PinPath,
			)

			loaded = append(loaded, loadedProg)
		}

		res.Programs = loaded
		return res, nil
	})
	if err != nil {
		if result.FailedOutcome.Status != "" {
			return displayOutcomeError(cli, err, result.FailedOutcome, &c.OutputFlags)
		}
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
