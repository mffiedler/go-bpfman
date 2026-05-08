package main

import (
	"github.com/frobware/go-bpfman/version"
)

// VersionCmd prints build version information.
type VersionCmd struct{}

func (cmd *VersionCmd) Run(c *CLI) error {
	return c.PrintOut(version.Get().Long())
}
