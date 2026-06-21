package main

import (
	"errors"
	"time"

	"github.com/frobware/go-bpfman/cmd/internal/args"
)

// ErrSilent is returned when the error has already been communicated
// (e.g., via JSON output) and cli.go should exit non-zero without
// printing an additional error message.
var ErrSilent = errors.New("silent error")

// MetadataFlags provides metadata-related flags.
type MetadataFlags struct {
	Metadata []args.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata to attach (can be repeated)."`
}

// AttachMetadataFlags carries -m/--metadata on attach commands,
// attaching user key/value labels to the created link.
type AttachMetadataFlags struct {
	Metadata []args.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE link metadata (can be repeated)."`
}

// GlobalDataFlags provides global data flags.
type GlobalDataFlags struct {
	GlobalData []args.GlobalData `short:"g" name:"global" help:"NAME=HEX global data (can be repeated)."`
}

// TTLFlag provides a TTL duration flag.
type TTLFlag struct {
	TTL time.Duration `help:"Time-to-live duration." default:"5m"`
}
