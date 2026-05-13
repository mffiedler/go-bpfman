// file builtin: writes a value to a private temporary file and
// returns its path as a scalar. The shared writeValueToTemp
// helper is also used by exec and start to materialise inline
// `file:$var` adapter args before spawning a subprocess.

package main

import (
	"fmt"
	"os"

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
	if len(args) == 0 || argText(args[0]) != "temp" {
		return shell.Value{}, fmt.Errorf("usage: file temp $var[.path] | [expr] | \"literal\"")
	}
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("file temp requires exactly one argument")
	}
	v, err := printValue(args[1])
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	path, err := writeValueToTemp(v)
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	if err := c.CLI.PrintOut(path + "\n"); err != nil {
		return shell.Value{}, err
	}
	return shell.StringValue(path), nil
}

// writeValueToTemp renders a shell.Value to a private temporary
// file and returns the absolute path. The file is created with
// mode 0600 in the OS default temp directory with a recognisable
// prefix. Used by file temp (this file), and by exec / start when
// resolving file:$var adapter args before spawning a subprocess.
func writeValueToTemp(v shell.Value) (string, error) {
	data, err := shell.RenderValue(v)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "bpfman-repl-")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	return f.Name(), nil
}
