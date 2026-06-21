package main

import (
	"github.com/frobware/go-bpfman/cmd/internal/args"
	"github.com/frobware/go-bpfman/manager"
)

func loadProgramSpecs(programs []args.ProgramSpec) []manager.ProgramSpec {
	out := make([]manager.ProgramSpec, len(programs))
	for i, prog := range programs {
		out[i] = manager.ProgramSpec{
			Name:       prog.Name,
			Type:       prog.Type,
			AttachFunc: prog.AttachFunc,
		}
	}
	return out
}
