package main

import (
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
)

func loadProgramSpecs(programs []bpfmancli.ProgramSpec) []manager.ProgramSpec {
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
