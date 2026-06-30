package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/cmd/bpfman/cliformat"
	"github.com/bpfman/bpfman/cmd/internal/args"
	"github.com/bpfman/bpfman/cmd/internal/runtime"
	"github.com/bpfman/bpfman/kernel"
	"github.com/bpfman/bpfman/manager"
	"github.com/bpfman/bpfman/platform"
)

// LoadImageCmd loads BPF programs from an OCI container image.
type LoadImageCmd struct {
	cliformat.OutputFlags
	MetadataFlags
	GlobalDataFlags

	// ImageURL is the OCI image reference to pull the bytecode from.
	ImageURL string `arg:"" name:"image" help:"OCI image reference (e.g., quay.io/bpfman-bytecode/xdp_pass:latest)."`

	// Programs selects which programs in the image to load, each given as
	// TYPE:NAME or TYPE:NAME:ATTACH_FUNC (comma-separated or repeated). For
	// fentry/fexit the ATTACH_FUNC component is required. When empty, every
	// program in the image is loaded.
	Programs []args.ProgramSpec `name:"programs" sep:"," help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (comma-separated or repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the image are loaded."`

	// PullPolicy controls when the image is pulled (Always, IfNotPresent,
	// or Never); it defaults to IfNotPresent.
	PullPolicy bpfman.ImagePullPolicy `short:"p" name:"pull-policy" help:"Image pull policy (Always, IfNotPresent, Never)." default:"IfNotPresent"`

	// RegistryAuth carries base64-encoded "username:password" registry
	// credentials for pulling the image. Prefer the BPFMAN_REGISTRY_AUTH
	// environment variable so the credentials do not appear in process
	// listings.
	RegistryAuth string `name:"registry-auth" env:"BPFMAN_REGISTRY_AUTH" help:"Base64-encoded registry auth (username:password). Prefer BPFMAN_REGISTRY_AUTH env var to avoid exposing credentials in process listings."`

	// Application groups the loaded programs under an application name,
	// stored as the bpfman.io/application metadata key.
	Application string `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`

	// MapOwnerID is the kernel program ID of an already-loaded program
	// whose maps these programs should share instead of creating their own.
	MapOwnerID kernel.ProgramID `name:"map-owner-id" help:"Program ID of another program to share maps with."`
}

// Run pulls the OCI image at ImageURL (honouring the pull policy and any
// registry credentials), loads the selected programs from it (applying
// metadata, global data, application grouping and any map-owner share),
// and renders the loaded programs in the chosen output format.
func (c *LoadImageCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	format, err := c.OutputFlags.Format()
	if err != nil {
		return err
	}

	logger := cli.Logger()

	mgr, cleanup, err := newImageManager(ctx, cli)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	logger.Info("loading BPF programs from OCI image", "image", c.ImageURL, "programs", len(c.Programs), "pull_policy", c.PullPolicy.String())

	// loadImageResult captures the result of a load image operation.
	type loadImageResult struct {
		Programs []bpfman.Program
	}

	// Manager.Load decides whether post-pull work needs the writer
	// flock: ordinary loads stay lockless, while explicit map-owner
	// joins and PinByName loads serialise internally.

	// Parse auth config from base64-encoded registry-auth.
	auth, err := registryAuthFromFlag(c.RegistryAuth)
	if err != nil {
		return err
	}

	if auth != nil {
		logger.Debug("using registry auth", "username", auth.Username)
	}

	var globalData map[string][]byte
	if len(c.GlobalData) > 0 {
		globalData = args.GlobalDataMap(c.GlobalData)
	}

	ref := platform.ImageRef{
		URL:        c.ImageURL,
		PullPolicy: c.PullPolicy,
		Auth:       auth,
	}

	req := manager.NewLoadRequest(manager.LoadSource{Image: &ref}, loadProgramSpecs(c.Programs), manager.LoadRequestOpts{
		UserMetadata: args.MetadataMap(c.Metadata),
		GlobalData:   globalData,
		Application:  c.Application,
		MapOwnerID:   c.MapOwnerID,
	})
	loaded, err := mgr.LoadFromRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to load from image: %w", err)
	}

	result := loadImageResult{Programs: loaded}

	return cliformat.RenderLoadedPrograms(cli.Out, cliformat.LoadedProgramsView{Programs: result.Programs}, format)
}

// registryAuthFromFlag decodes a base64-encoded registry-auth flag
// value into ImageAuth, returning nil when the flag is empty.
func registryAuthFromFlag(encoded string) (*platform.ImageAuth, error) {
	if encoded == "" {
		return nil, nil
	}
	username, password, err := parseRegistryAuth(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid registry-auth: %w", err)
	}

	return &platform.ImageAuth{
		Username: username,
		Password: password,
	}, nil
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
	if parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("username and password must both be non-empty")
	}

	return parts[0], parts[1], nil
}
