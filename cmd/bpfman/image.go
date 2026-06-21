package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/cmd/internal/runtime"
	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/internal/imagebuild"
	"github.com/frobware/go-bpfman/platform"
	platformebpf "github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/image/oci"
	"github.com/frobware/go-bpfman/platform/image/verify"
)

// ImageCmd groups image-related subcommands.
type ImageCmd struct {
	Build             ImageBuildCmd             `cmd:"" help:"Build and push an OCI image containing BPF bytecode."`
	GenerateBuildArgs ImageGenerateBuildArgsCmd `cmd:"" help:"Generate OCI image build arguments for BPF bytecode."`
	Inspect           ImageInspectCmd           `cmd:"" help:"Inspect OCI image metadata for BPF bytecode."`
	Verify            ImageVerifyCmd            `cmd:"" help:"Verify an OCI image signature."`
}

// ImageBuildCmd builds and pushes an OCI bytecode image.
type ImageBuildCmd struct {
	ImageURL string   `arg:"" name:"image" placeholder:"IMAGE" help:"Image reference to publish."`
	Bytecode []string `arg:"" optional:"" name:"bytecode" placeholder:"BYTECODE" help:"Bytecode input: BYTECODE for a single host-architecture image, or linux/arch=BYTECODE for a multi-architecture image."`

	CiliumEBPFProject string `short:"c" name:"cilium-ebpf-project" placeholder:"DIR" help:"Directory containing cilium/ebpf bpf2go object files."`
}

func (c *ImageBuildCmd) AllowRootless() bool { return true }

// ImageGenerateBuildArgsCmd prints the bytecode image build contract.
type ImageGenerateBuildArgsCmd struct {
	Output   string   `short:"o" name:"output" placeholder:"FORMAT" enum:"text,json" default:"text" help:"Output format: text or json."`
	Bytecode []string `arg:"" optional:"" name:"bytecode" placeholder:"BYTECODE" help:"Bytecode input: BYTECODE for a single host-architecture image, or linux/arch=BYTECODE for a multi-architecture image."`

	CiliumEBPFProject string `short:"c" name:"cilium-ebpf-project" placeholder:"DIR" help:"Directory containing cilium/ebpf bpf2go object files."`
}

func (c *ImageGenerateBuildArgsCmd) AllowRootless() bool { return true }

// ImageInspectCmd inspects an OCI bytecode image.
type ImageInspectCmd struct {
	ImageURL string `arg:"" name:"image" placeholder:"IMAGE" help:"OCI image reference to inspect."`
}

func (c *ImageInspectCmd) AllowRootless() bool { return true }

// Run executes the image build command.
func (c *ImageBuildCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	plan, err := c.plan()
	if err != nil {
		return err
	}

	logger := cli.Logger().With("tag", c.ImageURL)
	logger.Info("publishing bytecode image")
	published, err := oci.PublishBytecodeImage(ctx, c.ImageURL, plan, oci.WithPublishLogger(logger))
	if err != nil {
		return err
	}
	return cli.PrintOutf("published: %s\n", published.PinnedReference)
}

// Run executes the image generate-build-args command.
func (c *ImageGenerateBuildArgsCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	plan, err := c.plan()
	if err != nil {
		return err
	}

	output, err := imagebuild.Format(plan, c.Output)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// Run executes the image inspect command.
func (c *ImageInspectCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	inspection, err := oci.InspectBytecodeImage(ctx, c.ImageURL)
	if err != nil {
		return err
	}
	output, err := json.MarshalIndent(inspection, "", "  ")
	if err != nil {
		return err
	}
	return cli.PrintOut(string(output) + "\n")
}

func (c *ImageBuildCmd) plan() (imagebuild.Plan, error) {
	return planImageBuild(c.Bytecode, c.CiliumEBPFProject)
}

func (c *ImageGenerateBuildArgsCmd) plan() (imagebuild.Plan, error) {
	return planImageBuild(c.Bytecode, c.CiliumEBPFProject)
}

func planImageBuild(bytecode []string, ciliumProject string) (imagebuild.Plan, error) {
	source, err := bytecodeSource(bytecode, ciliumProject)
	if err != nil {
		return imagebuild.Plan{}, err
	}
	return imagebuild.Build(source, platformebpf.InspectBytecode)
}

func bytecodeSource(bytecode []string, ciliumProject string) (imagebuild.BytecodeSource, error) {
	if ciliumProject != "" {
		if len(bytecode) > 0 {
			return imagebuild.BytecodeSource{}, fmt.Errorf("--cilium-ebpf-project conflicts with bytecode inputs")
		}
		return platformebpf.CiliumProjectBytecodeSource(ciliumProject)
	}
	return positionalBytecodeSource(bytecode)
}

func positionalBytecodeSource(bytecode []string) (imagebuild.BytecodeSource, error) {
	var bare []string
	var mapped []imagebuild.BytecodeInput
	seenPlatforms := map[string]struct{}{}
	for _, arg := range bytecode {
		input, ok, err := parsePlatformBytecodeInput(arg)
		if err != nil {
			return imagebuild.BytecodeSource{}, err
		}
		if ok {
			if _, exists := seenPlatforms[input.Platform]; exists {
				return imagebuild.BytecodeSource{}, fmt.Errorf("platform %s specified more than once", input.Platform)
			}
			seenPlatforms[input.Platform] = struct{}{}
			mapped = append(mapped, input)
			continue
		}
		bare = append(bare, arg)
	}
	if len(mapped) > 0 {
		if len(bare) > 0 {
			return imagebuild.BytecodeSource{}, fmt.Errorf("cannot mix bare bytecode inputs with platform-mapped inputs")
		}
		return imagebuild.MultiArchSource(mapped)
	}
	if len(bare) > 1 {
		return imagebuild.BytecodeSource{}, fmt.Errorf("cannot infer OCI platforms from multiple BPF objects: BPF ELF records EM_BPF and endianness, not CPU architecture; pass one BYTECODE file, use linux/arch=BYTECODE inputs, or use --cilium-ebpf-project")
	}
	if len(bare) == 1 {
		return imagebuild.SingleFileSource(bare[0])
	}
	return imagebuild.MultiArchSource(nil)
}

func parsePlatformBytecodeInput(arg string) (imagebuild.BytecodeInput, bool, error) {
	platform, path, ok := strings.Cut(arg, "=")
	if !ok || !strings.Contains(platform, "/") {
		return imagebuild.BytecodeInput{}, false, nil
	}
	if path == "" {
		return imagebuild.BytecodeInput{}, false, fmt.Errorf("bytecode path is required for %s", platform)
	}
	input, ok := imagebuild.BytecodeInputForPlatform(platform, path)
	if !ok {
		return imagebuild.BytecodeInput{}, false, fmt.Errorf("unsupported OCI platform %q; supported platforms: %s", platform, strings.Join(imagebuild.SupportedPlatforms(), ", "))
	}
	return input, true, nil
}

// ImageVerifyCmd verifies the signature of an OCI image.
type ImageVerifyCmd struct {
	ImageURL string `arg:"" name:"image" placeholder:"IMAGE" help:"OCI image reference (e.g., quay.io/bpfman-bytecode/xdp_pass:latest)."`

	// Signing configuration
	AllowUnsigned               *bool   `name:"allow-unsigned" help:"Allow unsigned images (overrides config file)."`
	CertificateIdentity         *string `name:"certificate-identity" help:"Expected signing certificate identity (overrides config file)."`
	CertificateOIDCIssuer       *string `name:"certificate-oidc-issuer" help:"Expected signing certificate OIDC issuer (overrides config file)."`
	CertificateIdentityRegexp   *string `name:"certificate-identity-regexp" help:"Expected signing certificate identity regexp (overrides config file)."`
	CertificateOIDCIssuerRegexp *string `name:"certificate-oidc-issuer-regexp" help:"Expected signing certificate OIDC issuer regexp (overrides config file)."`

	// Registry authentication
	RegistryAuth string `name:"registry-auth" env:"BPFMAN_REGISTRY_AUTH" help:"Base64-encoded registry auth (username:password). Prefer BPFMAN_REGISTRY_AUTH env var to avoid exposing credentials in process listings."`
}

func (c *ImageVerifyCmd) AllowRootless() bool { return true }

// Run executes the image verify command.
func (c *ImageVerifyCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	logger := cli.Logger()
	logger.Info("verifying image signature", "image", c.ImageURL)

	// Load configuration (use CLI's config, not the deprecated --config flag)
	cfg, err := cli.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Apply CLI overrides
	if c.AllowUnsigned != nil {
		cfg.Signing.AllowUnsigned = *c.AllowUnsigned
	}
	if c.CertificateIdentity != nil && c.CertificateIdentityRegexp != nil {
		return fmt.Errorf("--certificate-identity and --certificate-identity-regexp are mutually exclusive")
	}
	if c.CertificateOIDCIssuer != nil && c.CertificateOIDCIssuerRegexp != nil {
		return fmt.Errorf("--certificate-oidc-issuer and --certificate-oidc-issuer-regexp are mutually exclusive")
	}
	registryAuth, err := registryAuthFromFlag(c.RegistryAuth)
	if err != nil {
		return err
	}

	override := config.TrustedIdentityConfig{}
	overrideGiven := false
	if c.CertificateIdentity != nil {
		override.CertificateIdentity = *c.CertificateIdentity
		overrideGiven = true
	}
	if c.CertificateOIDCIssuer != nil {
		override.CertificateOIDCIssuer = *c.CertificateOIDCIssuer
		overrideGiven = true
	}
	if c.CertificateIdentityRegexp != nil {
		override.CertificateIdentityRegexp = *c.CertificateIdentityRegexp
		overrideGiven = true
	}
	if c.CertificateOIDCIssuerRegexp != nil {
		override.CertificateOIDCIssuerRegexp = *c.CertificateOIDCIssuerRegexp
		overrideGiven = true
	}
	if overrideGiven {
		cfg.Signing.TrustedIdentities = []config.TrustedIdentityConfig{override}
	}

	// For verify command, always enable verification (that's the point)
	cfg.Signing.VerifyEnabled = true

	logger.Debug("signing configuration",
		"allow_unsigned", cfg.Signing.AllowUnsigned,
	)

	verifier, err := verify.FromSigningConfig(cfg.Signing, logger)
	if err != nil {
		return fmt.Errorf("configure signature verifier: %w", err)
	}

	req := platform.SignatureVerificationRequest{
		ImageRef: c.ImageURL,
		Auth:     registryAuth,
	}

	verification, err := verifier.Verify(ctx, req)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	switch verification.Status {
	case platform.SignatureVerificationVerified:
		return cli.PrintOutf("Image %s: signature verified\n", c.ImageURL)
	case platform.SignatureVerificationUnsignedAccepted:
		return cli.PrintOutf("Image %s: unsigned image accepted by policy\n", c.ImageURL)
	case platform.SignatureVerificationDisabled:
		return cli.PrintOutf("Image %s: signature verification disabled\n", c.ImageURL)
	default:
		return cli.PrintOutf("Image %s: signature policy accepted (%s)\n", c.ImageURL, verification.Status)
	}
}
