// Session-manipulating REPL commands: list variables (vars),
// define / list / remove aliases, remove variables, print a
// resolved value to stdout, and the internal value-lookup helper.
// None of these touch the bpfman manager; they all operate on the
// shell.Session and on the CLI writer.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/shell"
)

// replVars lists all session variables and their kinds. The
// kind is the OriginKind's string form (scalar, boolean,
// program, link, job, envelope, map, dispatcher, null,
// unknown), so a quick 'vars' tells the user what each binding
// is without forcing them to 'print $name' to find out.
func replVars(cli *bpfmancli.CLI, session *shell.Session) error {
	names := session.Names()
	var b strings.Builder
	for _, name := range names {
		v, _ := session.Get(name)
		fmt.Fprintf(&b, "  %s (%s)\n", name, v.Kind())
	}
	return cli.PrintOut(b.String())
}

// applyAlias rewrites the first token of an expanded arg slice if it
// matches a session alias. Expansion is non-recursive: only one
// rewrite is performed.
func applyAlias(session *shell.Session, args []shell.Arg) []shell.Arg {
	if len(args) == 0 {
		return args
	}
	w, ok := args[0].(shell.WordArg)
	if !ok {
		return args
	}
	expansion, found := session.GetAlias(w.Text)
	if !found {
		return args
	}
	rewritten := make([]shell.Arg, len(args))
	copy(rewritten, args)
	rewritten[0] = shell.WordArg{Text: expansion}
	return rewritten
}

// replAlias defines a first-token alias. Syntax: alias <name> = <expansion>.
// The name must not collide with shell commands or "bpfman".
func replAlias(cli *bpfmancli.CLI, session *shell.Session, args []string) error {
	if len(args) != 3 || args[1] != "=" {
		return fmt.Errorf("usage: alias <name> = <expansion>")
	}
	name, expansion := args[0], args[2]
	if _, ok := builtinRegistry[name]; ok {
		return fmt.Errorf("cannot alias %q: it is a shell command", name)
	}
	if name == "bpfman" {
		return fmt.Errorf("cannot alias %q: it is the domain prefix", name)
	}
	if name == "let" || name == "set" {
		return fmt.Errorf("cannot alias %q: it is a shell keyword", name)
	}
	session.SetAlias(name, expansion)
	return nil
}

// replUnalias removes one or more alias bindings.
func replUnalias(cli *bpfmancli.CLI, session *shell.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("unalias requires at least one alias name")
	}
	for _, name := range args {
		if _, ok := session.GetAlias(name); !ok {
			return fmt.Errorf("undefined alias %q", name)
		}
		session.DeleteAlias(name)
	}
	return nil
}

// replDefs lists all user-defined commands and their parameter lists.
func replDefs(cli *bpfmancli.CLI, session *shell.Session) error {
	names := session.DefNames()
	var b strings.Builder
	for _, name := range names {
		d, _ := session.GetDef(name)
		fmt.Fprintf(&b, "  %s(%s)\n", d.Name, strings.Join(d.Params, ", "))
	}
	return cli.PrintOut(b.String())
}

// replUndef removes one or more user-defined commands from the
// session.
func replUndef(session *shell.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("undef requires at least one def name")
	}
	for _, name := range args {
		if !session.DeleteDef(name) {
			return fmt.Errorf("undefined def %q", name)
		}
	}
	return nil
}

// replAliases lists all defined aliases.
func replAliases(cli *bpfmancli.CLI, session *shell.Session) error {
	names := session.AliasNames()
	var b strings.Builder
	for _, name := range names {
		expansion, _ := session.GetAlias(name)
		fmt.Fprintf(&b, "  %s = %s\n", name, expansion)
	}
	return cli.PrintOut(b.String())
}

// replUnset removes one or more variable bindings from the session.
func replUnset(cli *bpfmancli.CLI, session *shell.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("unset requires at least one variable name")
	}
	for _, name := range args {
		if _, ok := session.Get(name); !ok {
			return fmt.Errorf("undefined variable %q", name)
		}
		session.Delete(name)
	}
	return nil
}

// replPrint prints one or more values to stdout. Each argument is
// any expression that evaluates to a value: a variable reference
// ($var, $var.path), a quoted string, a bare word (literal string
// -- "print foo" prints the word "foo", not the variable foo), an
// arithmetic or comparison expression, or a threading expression
// ($x |> jq filter). To print a variable, write "print $foo".
//
// With a single argument the output matches the REPL's auto-print
// rendering -- scalars as plain text, structured values as
// indented JSON, absent values as "null", each followed by a
// newline -- so "print $r" and "$r" produce the same output. With
// multiple arguments each value renders compactly (scalar text,
// compact JSON, "null") and the pieces are joined by a single
// space and terminated by one newline, matching how Python and
// JavaScript spread multiple arguments across a print call.
func replPrint(cli *bpfmancli.CLI, args []shell.Arg) error {
	if len(args) == 0 {
		return fmt.Errorf(`print needs something to print: a variable like $name, a quoted string, or an expression`)
	}
	if len(args) == 1 {
		v, err := printValue(args[0])
		if err != nil {
			return err
		}
		return writeValue(cli, v)
	}
	parts := make([]string, len(args))
	for i, a := range args {
		v, err := printValue(a)
		if err != nil {
			return err
		}
		s, err := shell.RenderCompact(v)
		if err != nil {
			return fmt.Errorf("print: argument %d: %v", i+1, err)
		}
		parts[i] = s
	}
	return cli.PrintOut(strings.Join(parts, " ") + "\n")
}

// printValue resolves a single print argument into a shell.Value.
// Every arg kind is treated as a value: WordArg and QuotedArg are
// literal strings; ScalarValueArg and StructuredValueArg are
// already-resolved values from variable expansion or command
// substitution; AdapterArg carries its resolved Value.  Bare
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
	default:
		return shell.Value{}, fmt.Errorf("print: unsupported argument kind %T", arg)
	}
}

// writeValue renders a shell.Value onto cli: nil as "null", scalars
// as plain text, structured values as indented JSON. Shared between
// print and any other "print me this value" caller.
func writeValue(cli *bpfmancli.CLI, v shell.Value) error {
	if v.IsNil() {
		return cli.PrintOut("null\n")
	}
	if v.IsScalar() {
		s, err := v.Scalar()
		if err != nil {
			return err
		}
		return cli.PrintOut(s + "\n")
	}
	b, err := json.MarshalIndent(v.Raw(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	return cli.PrintOut(string(b) + "\n")
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
