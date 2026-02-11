package main

// LinkCmd groups link management subcommands.
type LinkCmd struct {
	Attach  AttachCmd       `cmd:"" help:"Attach a loaded program to a hook."`
	Detach  DetachCmd       `cmd:"" help:"Detach a link."`
	Get     GetLinkCmd      `cmd:"" help:"Get details of a link by link ID."`
	List    ListLinksCmd    `cmd:"" default:"withargs" help:"List managed links."`
	Delete  LinkDeleteCmd   `cmd:"" help:"Delete a link with cascading cleanup."`
	Explain ExplainLinksCmd `cmd:"" help:"Explain link fields and columns."`
}
