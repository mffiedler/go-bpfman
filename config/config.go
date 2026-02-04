// Package config handles bpfman daemon configuration.
//
// Configuration is loaded with overlay semantics:
//
//  1. Start with built-in defaults (embedded via go:embed from default.toml)
//  2. Overlay with config file values (if file exists)
//  3. CLI flags and environment variables override at runtime (handled by CLI layer)
//
// This ensures a valid configuration is always available, even when no
// config file exists. The TOML decoder only sets fields present in the
// file, leaving unspecified fields at their default values.
//
// If the config file exists but is invalid, Load returns an error rather
// than silently falling back to defaults.
package config

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed default.toml
var defaultConfigTOML string

const (
	// DefaultConfigPath is the default path to the bpfman config file.
	DefaultConfigPath = "/etc/bpfman/bpfman.toml"
)

// Config is the top-level bpfman configuration.
type Config struct {
	Signing SigningConfig `toml:"signing" json:"signing"`
	Logging LoggingConfig `toml:"logging" json:"logging"`
}

// LoggingConfig controls logging behaviour.
type LoggingConfig struct {
	// Level is the log spec (e.g., "info" or "info,manager=debug").
	Level string `toml:"level" json:"level,omitempty"`
	// Format is the output format: "text" or "json".
	Format string `toml:"format" json:"format,omitempty"`
	// Components provides an alternative way to specify per-component levels.
	Components map[string]string `toml:"components" json:"components,omitempty"`
}

// ToSpec converts the LoggingConfig to a log spec string.
// If Level is set, it takes precedence. Otherwise, Components are used.
func (c *LoggingConfig) ToSpec() string {
	if c.Level != "" {
		return c.Level
	}

	// Build spec from components
	if len(c.Components) == 0 {
		return ""
	}

	parts := make([]string, 0, len(c.Components)+1)
	parts = append(parts, "info") // default base level

	for component, level := range c.Components {
		parts = append(parts, component+"="+level)
	}

	return strings.Join(parts, ",")
}

// SigningConfig controls image signature verification.
// These settings match the Rust bpfman implementation.
type SigningConfig struct {
	// AllowUnsigned controls whether unsigned images are accepted.
	// When true (default), unsigned images can be loaded.
	// When false, all images must have valid signatures.
	AllowUnsigned bool `toml:"allow_unsigned" json:"allow_unsigned"`

	// VerifyEnabled controls whether signature verification is performed.
	// When true (default), images with signatures are verified.
	// When false, signature verification is skipped entirely.
	VerifyEnabled bool `toml:"verify_enabled" json:"verify_enabled"`
}

// DefaultConfig returns the default configuration from the embedded default.toml.
// This provides a valid baseline that is always available.
func DefaultConfig() Config {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		// This should never happen since default.toml is embedded at build time.
		// If it does, return a minimal safe config.
		return Config{
			Signing: SigningConfig{AllowUnsigned: true, VerifyEnabled: true},
			Logging: LoggingConfig{Level: "info", Format: "text"},
		}
	}
	return cfg
}

// Load reads configuration from a file path with overlay semantics.
//
// Behaviour:
//   - File missing: returns default configuration (no error)
//   - File exists and valid: overlays file values onto defaults
//   - File exists but invalid: returns error (fail fast)
//
// The TOML decoder only sets fields present in the file, so unspecified
// fields retain their default values from default.toml.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}

	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file is optional - use defaults
			return cfg, nil
		}
		return cfg, fmt.Errorf("failed to read config file: %w", err)
	}

	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg, nil
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	// Currently no cross-field validation needed
	return nil
}

// MustRequireSignatures returns true if all images must be signed.
func (c *SigningConfig) MustRequireSignatures() bool {
	return !c.AllowUnsigned && c.VerifyEnabled
}

// ShouldVerify returns true if signature verification should be performed.
func (c *SigningConfig) ShouldVerify() bool {
	return c.VerifyEnabled
}
