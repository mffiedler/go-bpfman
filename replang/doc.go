// Package replang implements the REPL language layer: tokenisation,
// variable binding, structured field access, and variable expansion.
//
// The package is pure -- it performs no I/O and depends only on the
// standard library. It provides the language mechanics that sit
// between the line editor and command dispatch: lexing input into
// tokens, parsing assignment vs plain command lines, storing
// structured values in a session, and expanding variable references
// to scalar strings.
package replang
