package main

import (
	"github.com/bpfman/bpfman/version"
)

// VersionCmd prints build version information.
type VersionCmd struct{}

// AllowRootless reports that the version command may run without root:
// it only prints build metadata and touches no kernel or bpffs state, so
// the CLI's root requirement is waived for it.
func (cmd *VersionCmd) AllowRootless() bool { return true }

// Run prints the long-form build version information (version, commit,
// build date and the like) to the CLI's output.
func (cmd *VersionCmd) Run(c *CLI) error {
	return c.PrintOut(version.Get().Long())
}
