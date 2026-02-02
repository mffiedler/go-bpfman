package cli

import (
	"context"
	"encoding/base64"
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

	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	runtime.Logger.Info("loading BPF programs from OCI image",
		"image", c.ImageURL,
		"programs", len(c.Programs),
		"pull_policy", c.PullPolicy.Value,
	)

	// loadImageResult captures both successful programs and any failure outcome.
	type loadImageResult struct {
		Programs      []bpfman.ManagedProgram
		FailedOutcome *outcome.ManagerOperationOutcome
	}

	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (loadImageResult, error) {
		var res loadImageResult

		// Build auth config from base64-encoded registry-auth
		var authConfig *interpreter.ImageAuth
		if c.RegistryAuth != "" {
			username, password, err := parseRegistryAuth(c.RegistryAuth)
			if err != nil {
				return res, fmt.Errorf("invalid registry-auth: %w", err)
			}
			runtime.Logger.Debug("using registry auth", "username", username)
			authConfig = &interpreter.ImageAuth{
				Username: username,
				Password: password,
			}
		}

		// Build image reference
		ref := interpreter.ImageRef{
			URL:        c.ImageURL,
			PullPolicy: pullPolicy,
			Auth:       authConfig,
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

		// Build ImageProgramSpecs for each program
		programs := make([]manager.ImageProgramSpec, 0, len(c.Programs))
		for _, spec := range c.Programs {
			progSpec := manager.ImageProgramSpec{
				ProgramName: spec.Name,
				ProgramType: spec.Type,
				AttachFunc:  spec.AttachFunc,
				GlobalData:  globalData,
				MapOwnerID:  c.MapOwnerID,
			}
			programs = append(programs, progSpec)
		}

		// Load via manager directly
		loadResult, err := runtime.Manager.LoadImage(ctx, runtime.Puller, ref, programs, manager.LoadImageOpts{
			UserMetadata: metadata,
			GlobalData:   globalData,
		})
		if err != nil {
			res.FailedOutcome = &loadResult.Outcome
			return res, fmt.Errorf("failed to load from image: %w", err)
		}

		for _, loaded := range loadResult.Programs {
			runtime.Logger.Info("program loaded successfully",
				"name", loaded.Kernel.Name,
				"kernel_id", loaded.Kernel.ID,
				"pin_path", loaded.Managed.PinPath,
			)
		}

		res.Programs = loadResult.Programs
		return res, nil
	})
	if err != nil {
		// On failure, display the outcome if available
		if result.FailedOutcome != nil {
			outcomeStr, fmtErr := FormatOutcome(*result.FailedOutcome, &c.OutputFlags)
			if fmtErr == nil {
				_ = cli.PrintErr(outcomeStr)
			}
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
