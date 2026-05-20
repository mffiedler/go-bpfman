// Package shell implements the REPL language layer: tokenisation,
// parsing to an AST, expression evaluation, variable binding, and
// structured field access.
//
// The package is pure — it performs no I/O and depends only on the
// standard library. It provides the language mechanics that sit
// between the line editor and command dispatch.
//
// # Pipeline
//
// Input flows through three stages:
//
//		source → Tokenise → []Token → Parse → *Program → EvalProgram
//
//	  - [Tokenise] produces a flat stream of [Token]s, each carrying
//	    its source line/column in a [Pos].
//	  - [Parse] builds a [Program] of typed AST nodes. Command
//	    substitutions are parsed eagerly so syntax errors inside [ ]
//	    surface at parse time.
//	  - [EvalProgram] walks the AST against an [Env] that bundles a
//	    [Session] with the callbacks that dispatch commands.
//
// # Statements
//
// The parser produces three statement variants, all reachable via
// the sealed [Stmt] interface:
//
//   - [LetStmt]: let name = expr — bind the result of evaluating
//     the RHS expression to a variable.
//   - [IfStmt]: if cond { ... } [elif cond { ... }]* [else { ... }]
//     — conditional branching, one primary branch plus any number
//     of elifs and an optional else.
//   - [CommandStmt]: a plain command invocation; the first arg
//     names the command.
//
// # Expressions
//
// Expression nodes are reachable via the sealed [Expr] interface:
//
//   - [LiteralExpr], [VarRefExpr], [AdapterExpr] -- primary forms.
//   - [UnaryExpr] — a single-operand predicate (true, false,
//     not-empty).
//   - [BinaryExpr] -- a two-operand comparison (==, !=, <, <=, >, >=).
//     Comparison semantics is selected by operand type rather than
//     by operator spelling: number-vs-number compares numerically,
//     string-vs-string textually, bool-vs-bool only supports == and
//     !=. Cross-type compares error rather than silently returning
//     false. See evalCompare in expr.go for the dispatch rules.
//
// Every AST node carries a [Pos] for diagnostic reporting.
//
// # Variable naming vs substitution
//
// A variable has a name and a value. The REPL distinguishes
// contexts that operate on the name from contexts that substitute
// the value, using the $ sigil to mark the boundary:
//
//   - Bare words name variables. The left-hand side of `let` uses
//     the variable name directly.
//   - The $ sigil substitutes a value. $prog means "replace this
//     with the value prog holds".
//   - Dotted paths reach into structure: $prog.record.program_id
//     traverses a structured value to reach a scalar. Braced syntax
//     ${prog.record.program_id} is also accepted.
//
// # Args as the dispatch boundary
//
// When the evaluator hands a command to [Env.ExecCommand] or
// [Env.ExecBind], it supplies a []Arg -- a typed
// post-evaluation representation whose variants preserve useful
// distinctions that a plain []string would lose:
//
//   - [WordArg]: literal command text (command names, flags, paths,
//     numeric IDs). Never came from a variable reference.
//   - [QuotedArg]: a quoted string literal, preserving the
//     syntactic distinction from unquoted words.
//   - [ScalarValueArg]: a value produced by variable expansion, now
//     resolved to a string. Covers both scalar variables ($count)
//     and dotted paths ($prog.record.program_id).
//   - [StructuredValueArg]: a bare reference to a structured
//     variable. The resolved [Value] is carried directly so command
//     parsers can apply typed extraction without re-parsing.
//   - [AdapterArg]: an inline adapter invocation (file:$var.path).
//
// This is the contract between the evaluator and its clients.
//
// # Session
//
// [Session] holds variable bindings and first-token aliases for the
// REPL. Values are stored as [Value], which may be scalar or
// structured. Variables live on a stack of frames: Set writes to
// the innermost frame, Get walks outward. DeleteLocal removes a
// binding from the innermost frame only; DeleteVisible walks
// outward and removes the first hit (the operation the
// user-facing `unset` builtin uses). PushFrame, PopFrame, and
// WithFrame manage block-shaped frame lifetimes. Names returns
// the visible variable set, deduplicated. Aliases are
// session-level and are managed via SetAlias, GetAlias,
// DeleteAlias, and AliasNames.
package shell
