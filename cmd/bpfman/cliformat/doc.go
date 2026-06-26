// Package cliformat renders bpfman CLI command results for display.
//
// Every command that prints output -- program load and list, link
// attach, get and list, dispatcher inspection -- routes its result
// through a Render function here, so the text and JSON encodings live
// in one place rather than being scattered across the command
// handlers. The Render functions (RenderProgram, RenderProgramList,
// RenderLinkList, RenderDispatcherSnapshot, and the rest) take a domain
// value or a view struct together with an OutputFormat and write either
// a human-readable table or JSON to the supplied writer.
//
// Tables are described declaratively by column registries
// (LinkColumnRegistry and friends): each ColumnInfo names a column and
// carries the accessor that extracts its cell, so a table's column set
// and ordering are data rather than format-string code. The view types
// (LinkListView, ProgramListView, and the others) adapt the manager's
// domain objects into the shape each table expects.
package cliformat
