package main

import (
	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/cmd/internal/args"
	"github.com/bpfman/bpfman/kernel"
)

// programIDs unwraps parsed CLI program-id arguments to the kernel
// values the manager and store accept.
func programIDs(parsed []args.ProgramID) []kernel.ProgramID {
	ids := make([]kernel.ProgramID, len(parsed))
	for i, p := range parsed {
		ids[i] = p.Value
	}
	return ids
}

// linkIDs unwraps parsed CLI link-id arguments to their bpfman values.
func linkIDs(parsed []args.LinkID) []bpfman.LinkID {
	ids := make([]bpfman.LinkID, len(parsed))
	for i, l := range parsed {
		ids[i] = l.Value
	}
	return ids
}
