package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

// ErrSilent is returned when the error has already been communicated
// (e.g., via JSON output) and cli.go should exit non-zero without
// printing an additional error message.
var ErrSilent = errors.New("silent error")

// MetadataFlags provides metadata-related flags.
type MetadataFlags struct {
	Metadata []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata to attach (can be repeated)."`
}

// AttachMetadataFlags carries -m/--metadata on attach commands. Link
// metadata is recognised for parity with the Rust CLI, but persisting
// it on links is not implemented yet; supplying it is rejected rather
// than silently discarded.
type AttachMetadataFlags struct {
	Metadata []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE link metadata (recognised for CLI compatibility; not implemented yet)."`
}

// errIfMetadataSet rejects supplied link metadata, since persistence on
// links is not implemented yet. It is called before any attach so the
// metadata cannot be silently lost.
func (f AttachMetadataFlags) errIfMetadataSet() error {
	if len(f.Metadata) > 0 {
		return fmt.Errorf("link metadata is not implemented yet")
	}
	return nil
}

// GlobalDataFlags provides global data flags.
type GlobalDataFlags struct {
	GlobalData []bpfmancli.GlobalData `short:"g" name:"global" help:"NAME=HEX global data (can be repeated)."`
}

// TTLFlag provides a TTL duration flag.
type TTLFlag struct {
	TTL time.Duration `help:"Time-to-live duration." default:"5m"`
}
