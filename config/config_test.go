package config

import (
	"reflect"
	"testing"

	"github.com/frobware/go-bpfman/logging"
)

func TestLoggingConfigToSpec_MergesComponents(t *testing.T) {
	cfg := LoggingConfig{
		Level: "info",
		Components: map[string]string{
			"manager": "debug",
			"store":   "warn",
		},
	}

	spec, err := logging.ParseSpec(cfg.ToSpec())
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	if spec.BaseLevel != logging.LevelInfo {
		t.Fatalf("base level = %v, want %v", spec.BaseLevel, logging.LevelInfo)
	}

	want := map[string]logging.Level{
		"manager": logging.LevelDebug,
		"store":   logging.LevelWarn,
	}
	if !reflect.DeepEqual(spec.Components, want) {
		t.Fatalf("components = %#v, want %#v", spec.Components, want)
	}
}

func TestLoggingConfigToSpec_DefaultBase(t *testing.T) {
	cfg := LoggingConfig{
		Components: map[string]string{
			"manager": "debug",
		},
	}

	spec, err := logging.ParseSpec(cfg.ToSpec())
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	if spec.BaseLevel != logging.LevelInfo {
		t.Fatalf("base level = %v, want %v", spec.BaseLevel, logging.LevelInfo)
	}
	if spec.Components["manager"] != logging.LevelDebug {
		t.Fatalf("component manager = %v, want %v", spec.Components["manager"], logging.LevelDebug)
	}
}

func TestLoggingConfigToSpec_Empty(t *testing.T) {
	cfg := LoggingConfig{}
	if got := cfg.ToSpec(); got != "" {
		t.Fatalf("spec = %q, want empty string", got)
	}
}

func TestConfigValidate_InvalidLoggingFormat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Logging.Format = "xml"

	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error for invalid logging format")
	}
}

func TestConfigValidate_InvalidLoggingSpec(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Logging.Components = map[string]string{
		"manager": "verbose",
	}

	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error for invalid logging level")
	}
}
