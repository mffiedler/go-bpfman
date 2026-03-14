// Package replang implements the REPL language layer: tokenisation,
// variable binding, structured field access, and variable expansion.
//
// The package is pure -- it performs no I/O and depends only on the
// standard library. It provides the language mechanics that sit
// between the line editor and command dispatch: lexing input into
// tokens, parsing assignment vs plain command lines, storing
// structured values in a session, and expanding variable references
// to scalar strings.
//
// # Variable naming vs substitution
//
// A variable has a name and a value. The REPL distinguishes between
// contexts that operate on the name and contexts that substitute the
// value, using the $ sigil to mark the boundary:
//
//   - Bare words name variables. Assignment targets (prog = load file
//     ...), inspection commands (dump prog), and listing (vars) all
//     use the variable name directly. No $ prefix is needed because
//     the command operates on the variable itself, not its value.
//
//   - The $ sigil substitutes a value. When a command expects a
//     concrete argument such as a program ID, $prog means "replace
//     this with the value that prog holds". The tokeniser produces a
//     VarRef token; Session.Expand resolves scalar references to
//     their string value. Structured values pass through as the
//     original $name token so that per-command handlers can extract
//     the relevant field (e.g. .record.program_id).
//
//   - Dotted paths reach into structure. $prog.record.program_id
//     traverses the structured value and resolves to a scalar.
//     Paths that land on a non-scalar intermediate are rejected.
//
// This convention mirrors common languages: Python uses bare names
// for binding and expressions but f"{x}" for interpolation; shell
// uses x=1 for assignment and $x for expansion.
package replang
