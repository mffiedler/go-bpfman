// exec builtin: spawn an external command. The two execution
// primitives (capture vs inherit-stdio) live in the repl package
// as repl.RunExternal and repl.RunExternalInherit; this file is
// the user-facing builtin that wires the statement-position
// inherit path to the dispatcher.

package main

import (
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// handleExec runs an external command at top-level statement
// position with stdio inherited from the parent: stdin from the
// terminal, stdout/stderr streamed live to the user's writers.
// Interactive programs (vi, less, ssh) get a real TTY; long-
// running programs (make, build) stream progress instead of
// buffering it. Non-zero exit becomes a returned *repl.ExecFailure
// so the chunk is reported as failed; launch failures (command
// not found, permission denied) propagate as plain errors. The
// bind path (`let r <- ls`) uses repl.RunExternal directly to
// capture into a BindResult.
func handleExec(c builtinCtx) (shell.Value, error) {
	return repl.RunExecStatement(c.Ctx, c.CLI, c.Args, c.Span)
}
