// file builtin: writes a value to a private temporary file and
// returns its path as a scalar. The temp-file rendering itself
// (repl.WriteValueToTemp) is mechanism, shared with the exec and
// start adapter paths that materialise `file:$var` arguments.

package main

import (
	"fmt"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// handleFile implements the file shell command. The only
// subcommand is "temp", which writes a REPL value to a private
// temporary file and returns the path as a scalar string. The
// single argument is any value-producing expression: a variable
// reference ($var, $var.path), a command substitution ([...]),
// or a quoted literal. A bare word is treated as a literal
// string -- to write the contents of a variable, pass $name, not
// the bare name.
func handleFile(c builtinCtx) (shell.Value, error) {
	args := c.Args
	if len(args) == 0 || repl.ArgText(args[0]) != "temp" {
		return shell.Value{}, fmt.Errorf("usage: file temp $var[.path] | [expr] | \"literal\"")
	}
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("file temp requires exactly one argument")
	}
	v, err := printValue(args[1])
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	path, err := repl.WriteValueToTemp(v)
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	if err := c.CLI.PrintOut(path + "\n"); err != nil {
		return shell.Value{}, err
	}
	return shell.StringValue(path), nil
}
