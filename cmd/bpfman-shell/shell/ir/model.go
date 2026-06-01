// IR types for the bpfman-shell lowered intermediate
// representation. This file defines only the data model. The
// dumper, the lowerer, and the interpreter sit in sibling files
// within the same package.
//
// The IR is block-structured. A Program has an entry
// block for the top-level script body, a list of every block
// reachable from that entry in emission order, and a list of
// user-defined commands. Each block holds a sequence of Instr
// values; the last instruction in a block is its terminator and
// names the next block(s) explicitly via *BasicBlock pointers.
//
// Instructions form a sealed sum via the unexported instrNode()
// marker, mirroring the existing Stmt and Expr discipline in
// parse.go and expr.go. Every instruction embeds a source.Span so the
// dump and the interpreter can point back at source coordinates
// without an out-of-band table.
//
// Eval, BuildArgs, and Assert carry lowered expression operands,
// so the runtime never reconstructs syntax.Expr trees to execute
// program behaviour.

package ir

import (
	"time"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"
)

// Program is the lowered form of a parsed *Program. It
// owns the top-level entry block, every block reachable from
// that entry in emission order, and every user-defined command
// declared in source order. The temp counter for the body
// records how many Temp values were allocated so the dumper
// and the interpreter can size their tables once.
type Program struct {
	Defs     []*Def
	Body     *BasicBlock
	Blocks   []*BasicBlock
	NumTemps int
	source.Span
}

// Def is the lowered form of a DefStmt: a named callable
// with an ordered parameter list, its own entry block, and its
// own block table. Params keeps the original parameter names
// for diagnostics; binding happens inside the def's entry block
// via the usual EnterFrame / BindName sequence.
type Def struct {
	Name      string
	Params    []string
	HasReturn bool
	Entry     *BasicBlock
	Blocks    []*BasicBlock
	NumTemps  int
	source.Span
}

// BasicBlock is a straight-line sequence of instructions ending
// in a terminator that names its successors via *BasicBlock
// pointers. The dumper assigns deterministic labels (bb0, bb1,
// ...) in emission order; blocks have no inherent identity
// beyond their pointer.
type BasicBlock struct {
	Instrs []Instr
	source.Span
}

// Temp identifies a temporary value produced inside a single
// Program body or Def body. Temp values are
// allocated in strict program order so the dumper can render
// them as t0, t1, ... without a separate name table. The
// containing unit's NumTemps field is the upper bound.
type Temp uint32

// Instr is the sealed sum of lowered instructions. Every
// concrete instruction is a pointer type so interpreters can
// share state via pointer identity if they ever need to, and
// so the dumper can pattern-match without value-copying source.Span
// fields.
type Instr interface {
	instrNode()
}

// FrameKind classifies an EnterFrame instruction. The kind
// records why the frame opened so the dumper, the interpreter,
// and any future stack-walker can speak about frames in the
// same vocabulary as the design documents.
type FrameKind int

const (
	FrameDef FrameKind = iota + 1
	FrameIfBranch
	FrameForEachIter
	FramePollAttempt
)

// EnterFrame opens a new runtime frame. Frames nest LIFO;
// ExitFrame closes the innermost one. The Kind field is
// observational, not semantic: the interpreter behaves the
// same regardless, but diagnostics and the dump rely on it.
type EnterFrame struct {
	Kind FrameKind
	source.Span
}

// ExitFrame closes the innermost frame. The lowerer is
// responsible for pairing EnterFrame and ExitFrame across
// every control path that can leave a frame, including the
// cleanup path that runs defers on the way out.
type ExitFrame struct {
	source.Span
}

// DeferScopeKind classifies an EnterDeferScope. Like
// FrameKind, this is for observability; the runtime semantics
// are the same across kinds, but the kinds drive which
// RunDefers policy applies when the scope unwinds.
type DeferScopeKind int

const (
	DeferScopeProgram DeferScopeKind = iota + 1
	DeferScopeDef
	DeferScopePollAttempt
)

// EnterDeferScope opens a new defer scope. Subsequent
// RegisterDefer instructions attach to this scope until the
// matching RunDefers fires.
type EnterDeferScope struct {
	Kind DeferScopeKind
	source.Span
}

// RegisterDefer queues an argv, built earlier into a Temp via
// BuildArgs, onto the innermost open defer scope. Argument
// values are frozen at registration time; the dispatch itself
// runs at scope exit, in LIFO order, using Policy to resolve
// the deferred head.
type RegisterDefer struct {
	Argv   Temp
	Policy DispatchPolicy
	Trace  bool
	source.Span
}

// RunDefersPolicy names how the scope unwinds. Program-level
// unwind runs at script exit; def-local unwind runs at
// function return; attempt-fatal unwind runs at the end of one
// poll attempt regardless of success or retry.
type RunDefersPolicy int

const (
	RunDefersProgram RunDefersPolicy = iota + 1
	RunDefersDefLocal
	RunDefersAttemptFatal
)

// RunDefers unwinds the innermost defer scope under the named
// policy. The instruction does not pop the frame; ExitFrame
// does that separately so the dump can show return-stash
// before cleanup ordering explicitly.
type RunDefers struct {
	Policy RunDefersPolicy
	source.Span
}

// Eval evaluates a lowered expression and stores the result in
// Dst. The instruction is named Eval because the IR context already
// makes expression lowering explicit; the dumper emits the design
// doc's illustrative spelling, eval_expr.
type Eval struct {
	Dst  Temp
	Expr Expr
	source.Span
}

// BuildArgs evaluates an ordered list of lowered argument
// expressions and packages them into a runtime argv stored in
// Dst.
type BuildArgs struct {
	Dst  Temp
	Args []Expr
	source.Span
}

// DispatchPolicy names the shell-level head-resolution policy an
// instruction uses. The policy is intentionally narrower than the
// full cmd-side runtime lane story: the IR records what the shell
// package itself guarantees (for example, "defs first, then
// ExecBind"), while the concrete builtin / driver / external split
// behind ExecBind or ExecCommand remains outside the IR boundary.
type DispatchPolicy int

const (
	DispatchPolicyDefThenExecBind DispatchPolicy = iota + 1
	DispatchPolicyDefThenExecCommand
)

// DispatchBind invokes the command named by Argv in bind
// position, producing a bind-result Temp that an ApplyBind can
// then consume. Bind dispatch may route to a builtin, a user
// def, or an external command; Policy names the shell-level
// resolution rule the interpreter must follow.
type DispatchBind struct {
	Dst         Temp
	Argv        Temp
	CallPos     source.Pos
	Policy      DispatchPolicy
	TraceHeader string
	source.Span
}

// DispatchCommand invokes the command named by Argv in
// statement position; no bind result is consumed. Failures
// flow through the program-level error policy rather than
// through an ApplyBind. Policy names the shell-level
// resolution rule the interpreter must follow.
type DispatchCommand struct {
	Argv   Temp
	Policy DispatchPolicy
	Trace  bool
	source.Span
}

// ApplyBind consumes the result of a DispatchBind and binds
// the named slots into the current frame. An empty Primary or
// Rc name means discard. When Guard is true the instruction
// branches to OnFail on a non-ok envelope instead of binding;
// non-guard binds always bind (the failure surfaces through Rc
// if named). OnFail is nil for non-guard binds.
//
// Argv references the same temp DispatchBind read so a guard
// failure raised in OnFail can carry the original argv to the
// diagnostic site. Without it the resulting *GuardFailure has
// no Args slot to populate and the engines drift on the error
// payload even when they agree on the halt decision.
type ApplyBind struct {
	Src     Temp
	Argv    Temp
	Primary string
	Rc      string
	Guard   bool
	OnFail  *BasicBlock
	source.Span
}

// BindName binds an identifier in the current frame to the
// value held in Src. Used for let, foreach iteration
// variables, and def parameter binding -- anywhere a name is
// introduced without a bind-position dispatch.
type BindName struct {
	Name        string
	Src         Temp
	TracePrefix string
	source.Span
}

// BindDestructure binds a positional name list against a list
// value held in Src. Each element of Names binds to the
// corresponding element of the list; an entry of "_" discards
// that slot. The interpreter validates list shape and length;
// the IR carries only the binding intent.
type BindDestructure struct {
	Names []string
	Src   Temp
	Trace bool
	source.Span
}

// BuildEnvelope synthesises a result envelope from literal
// fields. Used by lowering-time surfaces that need a concrete
// envelope to feed into EmitBindResult.
type BuildEnvelope struct {
	Dst  Temp
	Ok   bool
	Code int
	Err  string
	source.Span
}

// EmitBindResult publishes a bind result from a callee frame:
// the rc envelope and the primary value. A nil Rc means the
// interpreter should synthesise an ok envelope from program
// state; a nil Primary means no primary value is being
// published.
type EmitBindResult struct {
	Rc      *Temp
	Primary *Temp
	source.Span
}

// EmitResult forwards a value to the driver's PrintResult hook.
// Used for top-level expression statements in shell programs
// where a bare expression should auto-print the
// evaluated value. A nil PrintResult hook makes the instruction a
// no-op, matching the tree walker's ExprStmt rule.
type EmitResult struct {
	Src   Temp
	Trace bool
	source.Span
}

// TraceNote emits a one-line execution trace message when the
// driver's trace hook is enabled. It is execution metadata, not
// part of the canonical lowered dump: lowering uses it for
// control-flow constructs whose trace coverage is about branch or
// lifecycle choice rather than about a produced value.
type TraceNote struct {
	Msg string
	source.Span
}

// Stop halts the current execution unit.
type Stop struct {
	source.Span
}

// Jump transfers control unconditionally to Target. The
// terminator at the end of any block that does not need to
// branch, return, propagate, or stop.
type Jump struct {
	Target *BasicBlock
	source.Span
}

// Branch transfers control based on a boolean Temp.
type Branch struct {
	Cond  Temp
	True  *BasicBlock
	False *BasicBlock
	source.Span
}

// ReturnValue is the terminator that exits the enclosing def
// with Src as the published primary value. To names the def's
// shared epilogue block; the interpreter copies Src into a
// return slot and jumps to To, where def-local defers run
// before the frame closes. This preserves the documented
// return-stash-before-cleanup order while keeping every
// ReturnStmt in the source routed to a single cleanup block.
type ReturnValue struct {
	Src   Temp
	To    *BasicBlock
	Trace bool
	source.Span
}

// PropagateError exits the enclosing unit by re-raising the
// pending error. Used by guard failure paths after defers
// have run, when the caller's frame is the one that decides
// what happens next.
type PropagateError struct {
	source.Span
}

// PropagateGuardFailure raises a synthetic GuardFailure without
// going through DispatchBind / ApplyBind.
type PropagateGuardFailure struct {
	Primary string
	Head    string
	ArgSpan source.Span
	OK      bool
	Code    int
	Stdout  string
	Stderr  string
	Killed  bool
	Signal  string
	HasPID  bool
	PID     int
	source.Span
}

// BeginPoll opens a polling region. The interpreter enters Attempt
// first; explicit RetryPoll terminators loop back here until timeout,
// and ordinary failures remain fatal.
type BeginPoll struct {
	Timeout   time.Duration
	Every     time.Duration
	Attempt   *BasicBlock
	OnTimeout *BasicBlock
	OnSuccess *BasicBlock
	source.Span
}

// RetryPoll requests another poll attempt. Message names an optional
// Temp whose value is rendered on timeout as the last retry reason.
type RetryPoll struct {
	Message *Temp
	source.Span
}

// Fail is a structural lowering-time surface that becomes an
// explicit control-flow exit: a condition the lowerer can
// prove at compile time should never reach. Msg is the
// diagnostic the interpreter raises if it does.
type Fail struct {
	Msg string
	source.Span
}

// Assert evaluates one assertion clause at runtime and reports
// the outcome to the session's assertion policy via the Env. The
// IsRequire flag distinguishes 'require' (halt on failure) from
// 'assert' (record and continue). Lowered execution dispatches
// this directly through Env.ExecAssertIR.
type Assert struct {
	IsRequire bool
	Clause    AssertClause
	source.Span
}

// ForEach is the structured pseudo-terminator that opens a
// foreach loop. List names the Temp holding the evaluated list
// (it is the iterator's input). Names is the destructure shape
// applied to each element; len(Names) == 1 binds the element
// itself, len(Names) > 1 destructures it. Body is the block
// entered once per element and must end in a ForEachContinue
// terminator; Exit is the block control enters after the loop
// finishes naturally or via a break Jump.
type ForEach struct {
	List  Temp
	Names []string
	Body  *BasicBlock
	Exit  *BasicBlock
	source.Span
}

// ForEachContinue is the terminator at the end of a foreach
// body block. The interpreter pops every frame opened during
// this iteration -- including the iter frame itself --
// advances the iterator, and either re-enters the loop body
// (re-opening the iter frame and re-binding Names) or transfers
// to the loop's Exit block when exhausted. Continue statements
// in the source lower to a ForEachContinue terminator on a
// separate block.
type ForEachContinue struct {
	source.Span
}

// ExitLoop is the terminator a break statement lowers to. The
// interpreter pops every frame opened during the iteration --
// including the innermost loop's iter frame -- and transfers
// to that loop's Exit block. Break inside nested loops always
// targets the innermost loop; outer loops are unaffected.
type ExitLoop struct {
	source.Span
}

// RegisterDef installs a Def into the session. Canonical
// lowering now hoists top-level defs into Program.Defs and
// pre-registers them before body execution, so ordinary lowered
// output no longer emits RegisterDef. The instruction remains as
// an escape hatch for hand-built IR and tests that want an
// explicit def-publication step.
type RegisterDef struct {
	Def *Def
	source.Span
}

// ForEachCollect is the structured pseudo-terminator for a
// bind-collect: `let X <- foreach Y in $list { ... }`. The
// body executes once per element; its trailing CommandStmt
// dispatches in bind position and the result is collected
// into per-iteration accumulators for Primary and (optionally)
// Rc. Guard makes a non-ok envelope halt iteration and either
// publish the partial collection or unwind. On natural or
// guarded completion control transfers to Exit, where the
// interpreter performs the final BindName calls against the
// accumulated values.
type ForEachCollect struct {
	List    Temp
	Names   []string
	Primary string
	Rc      string
	Guard   bool
	Body    *BasicBlock
	Exit    *BasicBlock
	source.Span
}

// CollectProduce is the terminator at the end of a
// ForEachCollect body block. Result names the Temp produced
// by the trailing DispatchBind; the interpreter splits the
// envelope from the primary value and routes each into its
// accumulator before advancing the iterator.
type CollectProduce struct {
	Result    Temp
	FrameSpan source.Span
	source.Span
}

func (*EnterFrame) instrNode()            {}
func (*ExitFrame) instrNode()             {}
func (*EnterDeferScope) instrNode()       {}
func (*RegisterDefer) instrNode()         {}
func (*RunDefers) instrNode()             {}
func (*Eval) instrNode()                  {}
func (*BuildArgs) instrNode()             {}
func (*DispatchBind) instrNode()          {}
func (*DispatchCommand) instrNode()       {}
func (*ApplyBind) instrNode()             {}
func (*BindName) instrNode()              {}
func (*BuildEnvelope) instrNode()         {}
func (*EmitBindResult) instrNode()        {}
func (*EmitResult) instrNode()            {}
func (*TraceNote) instrNode()             {}
func (*Stop) instrNode()                  {}
func (*Jump) instrNode()                  {}
func (*Branch) instrNode()                {}
func (*ReturnValue) instrNode()           {}
func (*PropagateError) instrNode()        {}
func (*PropagateGuardFailure) instrNode() {}
func (*BeginPoll) instrNode()             {}
func (*RetryPoll) instrNode()             {}
func (*Fail) instrNode()                  {}
func (*Assert) instrNode()                {}
func (*ForEach) instrNode()               {}
func (*ForEachContinue) instrNode()       {}
func (*BindDestructure) instrNode()       {}
func (*ForEachCollect) instrNode()        {}
func (*CollectProduce) instrNode()        {}
func (*ExitLoop) instrNode()              {}
func (*RegisterDef) instrNode()           {}
