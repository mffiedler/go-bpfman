// Session-manipulating REPL commands: list variables (vars),
// define / list / remove aliases, remove variables, print a
// resolved value to stdout, and the internal value-lookup helper.
// None of these touch the bpfman manager; they all operate on the
// shell.Session and on the CLI writer.
//
// Each handler is a `handleX(builtinCtx) (Value, error)` function
// the builtin registry references directly. The unpacking from
// builtinCtx is inline -- session builtins are small enough that
// a separate `replX(args ...)` impl plus a one-line `handleX`
// adapter would be more boilerplate than substance.

package main

import (
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// handleVars lists all session variables and their kinds. The
// kind is the OriginKind's string form (scalar, boolean,
// program, link, job, result, map, dispatcher, null,
// unknown), so a quick 'vars' tells the user what each binding
// is without forcing them to 'print $name' to find out.
func handleVars(c builtinCtx) (shell.Value, error) {
	session := c.Env.Session
	names := session.Names()
	var b strings.Builder
	for _, name := range names {
		v, _ := session.Get(name)
		fmt.Fprintf(&b, "  %s (%s)\n", name, v.Kind())
	}
	return shell.Value{}, c.CLI.PrintOut(b.String())
}

// applyAlias moved to repl.ApplyAlias.

// handleAlias defines a first-token alias. Syntax:
// alias <name> = <expansion>. The name must not collide with shell
// commands or "bpfman".
func handleAlias(c builtinCtx) (shell.Value, error) {
	args := repl.ArgTexts(c.Args)
	if len(args) != 3 || args[1] != "=" {
		return shell.Value{}, fmt.Errorf("usage: alias <name> = <expansion>")
	}
	name, expansion := args[0], args[2]
	if _, ok := repl.Builtins()[name]; ok {
		return shell.Value{}, fmt.Errorf("cannot alias %q: it is a shell command", name)
	}
	if name == "bpfman" {
		return shell.Value{}, fmt.Errorf("cannot alias %q: it is the domain prefix", name)
	}
	if name == "let" || name == "set" {
		return shell.Value{}, fmt.Errorf("cannot alias %q: it is a shell keyword", name)
	}
	c.Env.Session.SetAlias(name, expansion)
	return shell.Value{}, nil
}

// handleUnalias removes one or more alias bindings.
func handleUnalias(c builtinCtx) (shell.Value, error) {
	args := repl.ArgTexts(c.Args)
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("unalias requires at least one alias name")
	}
	session := c.Env.Session
	for _, name := range args {
		if _, ok := session.GetAlias(name); !ok {
			return shell.Value{}, fmt.Errorf("undefined alias %q", name)
		}
		session.DeleteAlias(name)
	}
	return shell.Value{}, nil
}

// handleDefs lists all user-defined commands and their parameter
// lists.
func handleDefs(c builtinCtx) (shell.Value, error) {
	session := c.Env.Session
	names := session.DefNames()
	var b strings.Builder
	for _, name := range names {
		d, _ := session.GetDef(name)
		fmt.Fprintf(&b, "  %s(%s)\n", d.Name, strings.Join(d.Params, ", "))
	}
	return shell.Value{}, c.CLI.PrintOut(b.String())
}

// handleUndef removes one or more user-defined commands from the
// session.
func handleUndef(c builtinCtx) (shell.Value, error) {
	args := repl.ArgTexts(c.Args)
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("undef requires at least one def name")
	}
	session := c.Env.Session
	for _, name := range args {
		if !session.DeleteDef(name) {
			return shell.Value{}, fmt.Errorf("undefined def %q", name)
		}
	}
	return shell.Value{}, nil
}

// handleTrace toggles execution tracing. The shape is
// deliberately `trace on` / `trace off` (typed words, not
// bash-style flag glyphs) so the builtin reads as a sentence and
// avoids dragging back the retired `set var = val` form's name.
// Unknown arguments fail loudly so a typo cannot silently leave
// tracing in its previous state.
func handleTrace(c builtinCtx) (shell.Value, error) {
	args := repl.ArgTexts(c.Args)
	if len(args) != 1 {
		return shell.Value{}, fmt.Errorf("trace requires exactly one argument: on or off")
	}
	switch args[0] {
	case "on":
		c.Env.Session.SetTrace(true)
	case "off":
		c.Env.Session.SetTrace(false)
	default:
		return shell.Value{}, fmt.Errorf("trace: unknown argument %q (expected on or off)", args[0])
	}
	return shell.Value{}, nil
}

// handleAliases lists all defined aliases.
func handleAliases(c builtinCtx) (shell.Value, error) {
	session := c.Env.Session
	names := session.AliasNames()
	var b strings.Builder
	for _, name := range names {
		expansion, _ := session.GetAlias(name)
		fmt.Fprintf(&b, "  %s = %s\n", name, expansion)
	}
	return shell.Value{}, c.CLI.PrintOut(b.String())
}

// handleUnset removes one or more variable bindings from the
// session.
func handleUnset(c builtinCtx) (shell.Value, error) {
	args := repl.ArgTexts(c.Args)
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("unset requires at least one variable name")
	}
	session := c.Env.Session
	for _, name := range args {
		if _, ok := session.Get(name); !ok {
			return shell.Value{}, fmt.Errorf("undefined variable %q", name)
		}
		session.DeleteVisible(name)
	}
	return shell.Value{}, nil
}

// handlePrint prints one or more values to stdout. Each argument
// is any expression that evaluates to a value: a variable
// reference ($var, $var.path), a quoted string, a bare word
// (literal string -- "print foo" prints the word "foo", not the
// variable foo), an arithmetic or comparison expression, or a
// threading expression ($x |> jq filter). To print a variable,
// write "print $foo".
//
// With no arguments a single newline is emitted, matching the
// shape of Python's print(), JavaScript's console.log(), and
// shell echo -- handy for spacing output blocks apart.
//
// With a single argument the output matches the REPL's auto-print
// rendering -- scalars as plain text, structured values as
// indented JSON, absent values as "null", each followed by a
// newline -- so "print $r" and "$r" produce the same output. With
// multiple arguments each value renders compactly (scalar text,
// compact JSON, "null") and the pieces are joined by a single
// space and terminated by one newline, matching how Python and
// JavaScript spread multiple arguments across a print call.
func handlePrint(c builtinCtx) (shell.Value, error) {
	args := c.Args
	if len(args) == 0 {
		return shell.Value{}, c.CLI.PrintOut("\n")
	}
	if len(args) == 1 {
		v, err := printValue(args[0])
		if err != nil {
			return shell.Value{}, err
		}
		return shell.Value{}, repl.WriteValue(c.CLI, v)
	}
	parts := make([]string, len(args))
	for i, a := range args {
		v, err := printValue(a)
		if err != nil {
			return shell.Value{}, err
		}
		s, err := shell.RenderCompact(v)
		if err != nil {
			return shell.Value{}, fmt.Errorf("print: argument %d: %v", i+1, err)
		}
		parts[i] = s
	}
	return shell.Value{}, c.CLI.PrintOut(strings.Join(parts, " ") + "\n")
}

// printValue resolves a single print argument into a shell.Value.
// Every arg kind is treated as a value: WordArg and QuotedArg are
// literal strings; ScalarValueArg and StructuredValueArg are
// already-resolved values from variable expansion or command
// substitution; AdapterArg carries its resolved Value. Bare
// identifiers are NOT looked up in the session -- callers must
// write $name to dereference a variable.
func printValue(arg shell.Arg) (shell.Value, error) {
	switch a := arg.(type) {
	case shell.WordArg:
		return shell.StringValue(a.Text), nil
	case shell.QuotedArg:
		return shell.StringValue(a.Text), nil
	case shell.ScalarValueArg:
		return shell.StringValue(a.Text), nil
	case shell.StructuredValueArg:
		return a.Value, nil
	case shell.AdapterArg:
		return a.Value, nil
	case shell.NilArg:
		// A null value at the print boundary renders as the JSON
		// token "null", matching what gojq and other JSON tools
		// produce. This is intentionally lossy: print is a
		// human-facing observation tool, not a typed value
		// transformer.
		return shell.Value{}, nil
	default:
		return shell.Value{}, fmt.Errorf("print: unsupported argument kind %T", arg)
	}
}

// lookupBareVar resolves a bare variable name (no $ prefix) with an
// optional dotted path into a shell.Value. This is the shared logic
// used by print and assert nil.
func lookupBareVar(session *shell.Session, arg string) (shell.Value, error) {
	varName := arg
	path := ""
	if i := strings.IndexAny(arg, ".["); i >= 0 {
		varName = arg[:i]
		path = arg[i:]
		path = strings.TrimPrefix(path, ".")
	}

	v, ok := session.Get(varName)
	if !ok {
		return shell.Value{}, fmt.Errorf("undefined variable %q", varName)
	}

	if path != "" {
		return v.LookupValue(varName, path)
	}
	return v, nil
}
