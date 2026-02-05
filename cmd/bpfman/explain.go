package main

// ExplainCmd is the root command for explaining resource schemas and columns.
type ExplainCmd struct {
	Programs ExplainProgramsCmd `cmd:"programs" help:"Explain program fields and columns."`
	Links    ExplainLinksCmd    `cmd:"links" help:"Explain link fields and columns."`
}
