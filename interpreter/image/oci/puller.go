// Package oci implements OCI image pulling for BPF bytecode.
package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/interpreter"
)

const (
	// LabelPrograms is the OCI label containing program metadata.
	LabelPrograms = "io.ebpf.programs"

	// LabelMaps is the OCI label containing map metadata.
	LabelMaps = "io.ebpf.maps"
)

// cachedMetadata stores image metadata in the cache directory.
type cachedMetadata struct {
	Digest   string            `json:"digest"`
	Programs map[string]string `json:"programs,omitempty"`
	Maps     map[string]string `json:"maps,omitempty"`
	PulledAt time.Time         `json:"pulled_at"`
}

// puller implements ImagePuller using ORAS for OCI registry access.
type puller struct {
	cache    bpfmanfs.ImageCache
	logger   *slog.Logger
	verifier interpreter.SignatureVerifier
}

// Option configures a puller.
type Option func(*puller) error

// WithLogger sets the logger.
func WithLogger(logger *slog.Logger) Option {
	return func(p *puller) error {
		p.logger = logger
		return nil
	}
}

// WithVerifier sets the signature verifier.
// If not set, no signature verification is performed.
func WithVerifier(v interpreter.SignatureVerifier) Option {
	return func(p *puller) error {
		p.verifier = v
		return nil
	}
}

// NewPuller creates a new OCI image puller.
// The cache parameter specifies where pulled images are stored; obtain it
// via FSLayout.ImageCache().
func NewPuller(cache bpfmanfs.ImageCache, opts ...Option) (interpreter.ImagePuller, error) {
	if !cache.Valid() {
		return nil, fmt.Errorf("invalid image cache")
	}

	p := &puller{
		cache:  cache,
		logger: slog.Default(),
	}

	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}

	p.logger.Debug("initialising OCI puller", "cache_dir", p.cache.Root())

	if err := p.cache.EnsureRoot(); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	return p, nil
}

// Pull downloads an image and returns the extracted bytecode.
func (p *puller) Pull(ctx context.Context, ref interpreter.ImageRef) (interpreter.PulledImage, error) {
	logger := p.logger.With("url", ref.URL, "policy", ref.PullPolicy.String())
	logger.Info("pulling OCI image")

	// Compute cache key from URL
	cacheKey := p.cache.CacheKey(ref.URL)
	logger = logger.With("cache_key", cacheKey)

	// Check cache based on pull policy
	if ref.PullPolicy != bpfman.PullAlways {
		if cached, ok := p.checkCache(cacheKey, ref, logger); ok {
			logger.Info("using cached image", "digest", cached.Digest)
			return cached, nil
		}

		if ref.PullPolicy == bpfman.PullNever {
			logger.Error("image not in cache and pull policy is Never")
			return interpreter.PulledImage{}, fmt.Errorf("image %s not in cache and pull policy is Never", ref.URL)
		}
	}

	logger.Debug("pulling image from registry")

	// Parse the reference
	repo, err := remote.NewRepository(ref.URL)
	if err != nil {
		logger.Error("failed to parse image reference", "error", err)
		return interpreter.PulledImage{}, fmt.Errorf("failed to parse image reference: %w", err)
	}

	// Set up authentication
	if err := p.configureAuth(repo, ref.Auth, logger); err != nil {
		return interpreter.PulledImage{}, err
	}

	logger.Debug("resolving image manifest")

	// Resolve the manifest descriptor
	desc, err := repo.Resolve(ctx, repo.Reference.Reference)
	if err != nil {
		logger.Error("failed to resolve image", "error", err)
		return interpreter.PulledImage{}, fmt.Errorf("failed to resolve image: %w", err)
	}

	logger.Info("image resolved", "digest", desc.Digest.String(), "media_type", desc.MediaType)

	// Verify image signature if a verifier is configured
	if p.verifier != nil {
		// Use the resolved digest for verification to ensure we verify what we pull
		verifyRef := ref.URL
		if desc.Digest != "" {
			// Append digest to ensure we verify the exact image
			verifyRef = ref.URL + "@" + desc.Digest.String()
		}
		if err := p.verifier.Verify(ctx, verifyRef); err != nil {
			logger.Error("image signature verification failed", "error", err)
			return interpreter.PulledImage{}, fmt.Errorf("signature verification failed: %w", err)
		}
		logger.Info("image signature verified")
	}

	// Handle OCI image index (multi-platform manifest list)
	manifestDesc := desc
	if desc.MediaType == "application/vnd.oci.image.index.v1+json" ||
		desc.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		logger.Debug("image is a manifest list, selecting platform")
		platformDesc, err := p.selectPlatform(ctx, repo, desc, logger)
		if err != nil {
			return interpreter.PulledImage{}, err
		}
		manifestDesc = platformDesc
		logger.Info("selected platform manifest", "digest", manifestDesc.Digest.String())
	}

	// Fetch the manifest
	rc, err := repo.Manifests().Fetch(ctx, manifestDesc)
	if err != nil {
		logger.Error("failed to fetch manifest", "error", err)
		return interpreter.PulledImage{}, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	manifestContent, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to read manifest: %w", err)
	}

	// Parse manifest to find layers and config
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	logger.Info("manifest parsed", "layers", len(manifest.Layers))

	if len(manifest.Layers) == 0 {
		return interpreter.PulledImage{}, fmt.Errorf("image has no layers")
	}

	// Extract labels from config
	programs, maps, err := p.extractLabels(ctx, repo, manifest.Config.Digest, logger)
	if err != nil {
		logger.Warn("failed to extract labels", "error", err)
	}

	logger.Debug("extracted image labels", "programs", programs, "maps", maps)

	// Fetch the first layer (should contain the bytecode)
	layer := manifest.Layers[0]
	logger.Info("fetching bytecode layer", "digest", layer.Digest, "size", layer.Size, "media_type", layer.MediaType)

	layerDesc := ocispec.Descriptor{
		MediaType: layer.MediaType,
		Digest:    digest.Digest(layer.Digest),
		Size:      layer.Size,
	}
	layerRC, err := repo.Blobs().Fetch(ctx, layerDesc)
	if err != nil {
		logger.Error("failed to fetch layer", "error", err)
		return interpreter.PulledImage{}, fmt.Errorf("failed to fetch layer: %w", err)
	}
	layerContent, err := io.ReadAll(layerRC)
	layerRC.Close()
	if err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to read layer: %w", err)
	}

	logger.Info("layer fetched", "size", len(layerContent))

	// Create temp directory for extraction
	tempDir, cleanup, err := p.cache.CreateTempDir()
	if err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer cleanup()

	// Write layer content to temp file
	layerFile := filepath.Join(tempDir, "layer.blob")
	if err := p.cache.WriteTempFile(tempDir, "layer.blob", layerContent); err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to write layer: %w", err)
	}
	_ = layerFile // used by extractBytecode via the tempDir

	// Extract bytecode from the layer
	bytecodeFile, err := extractBytecode(tempDir, logger)
	if err != nil {
		return interpreter.PulledImage{}, err
	}

	// Create cache directory and move bytecode
	if err := p.cache.EnsureCacheDir(cacheKey); err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to create cache directory: %w", err)
	}

	if err := p.cache.CacheBytecode(cacheKey, bytecodeFile); err != nil {
		return interpreter.PulledImage{}, fmt.Errorf("failed to cache bytecode: %w", err)
	}

	destPath := p.cache.BytecodePath(cacheKey)
	logger.Debug("bytecode cached", "path", destPath)

	// Validate the ELF file
	if err := validateELF(destPath, logger); err != nil {
		// Clean up invalid file
		if rmErr := p.cache.RemoveCacheEntry(cacheKey); rmErr != nil {
			logger.Warn("failed to remove cache directory during cleanup", "cache_key", cacheKey, "error", rmErr)
		}
		return interpreter.PulledImage{}, err
	}

	resolvedDigest := manifestDesc.Digest.String()

	// Save metadata
	meta := cachedMetadata{
		Digest:   resolvedDigest,
		Programs: programs,
		Maps:     maps,
		PulledAt: time.Now(),
	}

	if err := p.cache.SaveMetadata(cacheKey, meta); err != nil {
		logger.Warn("failed to save metadata", "error", err)
		// Not fatal - continue
	}

	logger.Info("image cached successfully", "path", destPath)

	return interpreter.PulledImage{
		ObjectPath: destPath,
		Programs:   programs,
		Maps:       maps,
		URL:        ref.URL,
		Digest:     resolvedDigest,
		PullPolicy: ref.PullPolicy,
	}, nil
}

// checkCache checks if a valid cached image exists.
func (p *puller) checkCache(cacheKey string, ref interpreter.ImageRef, logger *slog.Logger) (interpreter.PulledImage, bool) {
	// Check if bytecode exists
	if !p.cache.BytecodeExists(cacheKey) {
		logger.Debug("cache miss: bytecode not found")
		return interpreter.PulledImage{}, false
	}

	// Try to load metadata
	var meta cachedMetadata
	if err := p.cache.LoadMetadata(cacheKey, &meta); err != nil {
		logger.Debug("cache miss: metadata not found", "error", err)
		return interpreter.PulledImage{}, false
	}

	logger.Debug("cache hit", "digest", meta.Digest, "pulled_at", meta.PulledAt)

	return interpreter.PulledImage{
		ObjectPath: p.cache.BytecodePath(cacheKey),
		Programs:   meta.Programs,
		Maps:       meta.Maps,
		URL:        ref.URL,
		Digest:     meta.Digest,
		PullPolicy: ref.PullPolicy,
	}, true
}

// configureAuth sets up authentication for the repository.
func (p *puller) configureAuth(repo *remote.Repository, authConfig *interpreter.ImageAuth, logger *slog.Logger) error {
	// If explicit credentials provided, use them
	if authConfig != nil && authConfig.Username != "" {
		logger.Debug("using explicit credentials", "username", authConfig.Username)
		repo.Client = &auth.Client{
			Client: retry.DefaultClient,
			Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
				Username: authConfig.Username,
				Password: authConfig.Password,
			}),
		}
		return nil
	}

	// Try to load credentials from credential stores
	credStore, err := newCredentialStore(logger)
	if err != nil {
		logger.Debug("no credential store found, using anonymous access", "error", err)
		return nil
	}

	logger.Debug("using credential store for authentication")
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Credential: credentials.Credential(credStore),
	}

	return nil
}

// newCredentialStore creates a credential store checking Podman and Docker locations.
func newCredentialStore(logger *slog.Logger) (credentials.Store, error) {
	// Try Podman locations first
	podmanPaths := []string{}

	if xdgRuntime := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntime != "" {
		podmanPaths = append(podmanPaths, filepath.Join(xdgRuntime, "containers/auth.json"))
	}

	if home := os.Getenv("HOME"); home != "" {
		podmanPaths = append(podmanPaths, filepath.Join(home, ".config/containers/auth.json"))
	}

	for _, path := range podmanPaths {
		if _, err := os.Stat(path); err == nil {
			logger.Debug("found Podman credentials", "path", path)
			return credentials.NewStore(path, credentials.StoreOptions{})
		}
	}

	// Fall back to Docker
	logger.Debug("trying Docker credential store")
	return credentials.NewStoreFromDocker(credentials.StoreOptions{})
}

// selectPlatform selects the appropriate platform manifest from an image index.
func (p *puller) selectPlatform(ctx context.Context, repo *remote.Repository, indexDesc ocispec.Descriptor, logger *slog.Logger) (ocispec.Descriptor, error) {
	rc, err := repo.Manifests().Fetch(ctx, indexDesc)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to fetch index: %w", err)
	}
	defer rc.Close()

	indexContent, err := io.ReadAll(rc)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to read index: %w", err)
	}

	var index struct {
		Manifests []struct {
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
			MediaType string `json:"mediaType"`
			Platform  struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexContent, &index); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to parse index: %w", err)
	}

	if len(index.Manifests) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("image index has no manifests")
	}

	// Get current architecture
	hostArch := getHostArch()
	logger.Debug("selecting platform", "host_arch", hostArch, "available", len(index.Manifests))

	// Try to find matching architecture
	for _, m := range index.Manifests {
		logger.Debug("checking manifest", "arch", m.Platform.Architecture, "os", m.Platform.OS, "digest", m.Digest)
		if m.Platform.Architecture == hostArch && m.Platform.OS == "linux" {
			return ocispec.Descriptor{
				MediaType: m.MediaType,
				Digest:    digest.Digest(m.Digest),
				Size:      m.Size,
			}, nil
		}
	}

	// Fall back to first manifest
	m := index.Manifests[0]
	logger.Warn("no matching platform found, using first manifest",
		"host_arch", hostArch,
		"first_arch", m.Platform.Architecture)
	return ocispec.Descriptor{
		MediaType: m.MediaType,
		Digest:    digest.Digest(m.Digest),
		Size:      m.Size,
	}, nil
}

// getHostArch returns the host architecture in OCI format.
func getHostArch() string {
	// Map Go GOARCH to OCI architecture names
	switch arch := os.Getenv("GOARCH"); arch {
	case "":
		// Detect at runtime
		return detectArch()
	default:
		return goArchToOCI(arch)
	}
}

// detectArch detects the current architecture.
func detectArch() string {
	// Use runtime detection via uname or similar
	// For now, use a compile-time constant via build tags would be better,
	// but we'll use a simple heuristic
	return goArchToOCI(runtime.GOARCH)
}

// goArchToOCI converts Go architecture names to OCI format.
func goArchToOCI(goArch string) string {
	switch goArch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	case "386":
		return "386"
	case "ppc64le":
		return "ppc64le"
	case "s390x":
		return "s390x"
	default:
		return goArch
	}
}

// extractLabels fetches the image config blob and extracts BPF labels.
// configDigest should be the digest of the config blob from the manifest.
func (p *puller) extractLabels(ctx context.Context, repo *remote.Repository, configDigest string, logger *slog.Logger) (programs, maps map[string]string, err error) {
	if configDigest == "" {
		logger.Debug("no config digest provided, skipping label extraction")
		return nil, nil, nil
	}

	logger.Debug("fetching config for labels", "config_digest", configDigest)

	// Fetch the config blob directly
	rc, err := repo.Fetch(ctx, ocispec.Descriptor{Digest: digest.Digest(configDigest)})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch config: %w", err)
	}
	defer rc.Close()

	configContent, err := io.ReadAll(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read config: %w", err)
	}

	// Parse config to get labels
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(configContent, &config); err != nil {
		return nil, nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Extract program labels
	if progJSON := config.Config.Labels[LabelPrograms]; progJSON != "" {
		programs = make(map[string]string)
		if err := json.Unmarshal([]byte(progJSON), &programs); err != nil {
			logger.Warn("failed to parse programs label", "error", err)
		}
	}

	// Extract map labels
	if mapJSON := config.Config.Labels[LabelMaps]; mapJSON != "" {
		maps = make(map[string]string)
		if err := json.Unmarshal([]byte(mapJSON), &maps); err != nil {
			logger.Warn("failed to parse maps label", "error", err)
		}
	}

	return programs, maps, nil
}
