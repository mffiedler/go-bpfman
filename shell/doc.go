// Package shell implements the REPL language layer: tokenisation,
// statement parsing, variable binding, structured field access, and
// typed argument expansion.
//
// The package is pure -- it performs no I/O and depends only on the
// standard library. It provides the language mechanics that sit
// between the line editor and command dispatch.
//
// # Statements
//
// Input lines are tokenised by [Tokenise] and parsed by [ParseStmt]
// into one of three statement variants:
//
//   - [LetStmt]: let name = command...
//     Binds the result of a command to a variable. The command tokens
//     are passed to the client for execution; the client stores the
//     result back into the session.
//
//   - [SetStmt]: set name = value
//     Binds a scalar literal to a variable. The value is a single
//     token (word, quoted string, or variable reference).
//
//   - [CommandStmt]: command arg...
//     A plain command with no variable binding.
//
// # Variable naming vs substitution
//
// A variable has a name and a value. The REPL distinguishes between
// contexts that operate on the name and contexts that substitute the
// value, using the $ sigil to mark the boundary:
//
//   - Bare words name variables. The left-hand side of let and set
//     uses the variable name directly. No $ prefix is needed because
//     the statement operates on the variable itself, not its value.
//
//   - The $ sigil substitutes a value. When a command expects a
//     concrete argument such as a program ID, $prog means "replace
//     this with the value that prog holds".
//
//   - Dotted paths reach into structure. $prog.record.program_id
//     traverses the structured value and resolves to a scalar.
//     Braced syntax ${prog.record.program_id} is also accepted.
//
// # Expansion and the Arg types
//
// [Session.Expand] resolves variable references in a token slice and
// returns []Arg, a typed representation of the expanded arguments.
// The [Arg] interface is a sealed sum type with four variants:
//
//   - [WordArg]: literal command text (command names, flags, paths,
//     numeric IDs). Never came from a variable reference.
//
//   - [QuotedArg]: a quoted string literal, preserving the syntactic
//     distinction from unquoted words.
//
//   - [ScalarValueArg]: a value produced by variable expansion. The
//     original variable reference has been resolved to a string. This
//     covers both scalar variables ($count) and dotted paths into
//     structured values ($prog.record.program_id).
//
//   - [StructuredValueArg]: a bare reference to a structured variable
//     ($prog with no field path). The resolved [Value] is carried
//     directly so that command parsers can extract the relevant field
//     without re-parsing dollar prefixes or calling session.Get.
//
// This is the contract between shell and its clients. Clients
// receive typed, structured arguments and never need to re-discover
// variable references from strings.
//
// # Session
//
// [Session] holds variable bindings for the REPL. Values are stored
// as [Value], which may be scalar (a single string) or structured
// (a JSON-like tree with optional origin metadata). The session
// provides Set, Get, Delete, and Names for variable management.
package shell
