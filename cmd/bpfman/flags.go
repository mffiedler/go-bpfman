package main

import (
	"github.com/bpfman/bpfman/cmd/internal/args"
)

// MetadataFlags provides metadata-related flags.
type MetadataFlags struct {
	// Metadata holds repeatable -m/--metadata KEY=VALUE pairs
	// recorded as program metadata at load time.
	Metadata []args.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata to attach (can be repeated)."`
}

// AttachMetadataFlags carries -m/--metadata on attach commands,
// attaching user key/value labels to the created link.
type AttachMetadataFlags struct {
	// Metadata holds repeatable -m/--metadata KEY=VALUE labels
	// recorded on the created link.
	Metadata []args.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE link metadata (can be repeated)."`
}

// GlobalDataFlags provides global data flags.
type GlobalDataFlags struct {
	// GlobalData holds repeatable -g/--global NAME=HEX values used to
	// populate the program's global variables at load time.
	GlobalData []args.GlobalData `short:"g" name:"global" help:"NAME=HEX global data (can be repeated)."`
}
