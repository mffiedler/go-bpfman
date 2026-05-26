// Package bpfmanbuiltin implements the `bpfman ...` builtin family.
//
// It is the shell's frontend for the product domain commands:
// program, link, dispatcher, and audit. The package parses the
// builtin's subgrammar and dispatches either to the in-process
// library path or to the external binary path, depending on the
// configured mode.
package bpfmanbuiltin
