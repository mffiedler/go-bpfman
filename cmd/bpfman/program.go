package main

// ProgramCmd groups program management subcommands.
type ProgramCmd struct {
	Load    LoadCmd            `cmd:"" help:"Load a BPF program from an object file or image."`
	Unload  UnloadCmd          `cmd:"" help:"Unload a managed BPF program."`
	Get     GetProgramCmd      `cmd:"" help:"Get details of a program by program ID."`
	List    ListProgramsCmd    `cmd:"" default:"withargs" help:"List managed programs."`
	Delete  ProgramDeleteCmd   `cmd:"" help:"Delete a program with cascading cleanup."`
	Explain ExplainProgramsCmd `cmd:"" help:"Explain program fields and columns."`
}
