package operation

import (
	"context"

	"github.com/frobware/go-bpfman/outcome"
)

// nodeFlavour distinguishes the four node types in a plan.
type nodeFlavour uint8

const (
	flavourValidate nodeFlavour = iota
	flavourProduce
	flavourDo
	flavourTry
)

// node is the internal representation of a plan step. Callers
// construct nodes via the Validate, Produce, Do, and Try functions;
// the struct fields are unexported.
type node struct {
	label   string
	flavour nodeFlavour
	kind    outcome.StepKind
	target  string

	// Exactly one of these is set, depending on flavour.
	// validate/do/try use execFn; produce uses produceFn.
	execFn    func(context.Context, *Bindings) error
	produceFn func(context.Context, *Bindings) (any, error)

	// For produce: the key name used to store the binding.
	bindKey string

	// Options (only meaningful for produce/do).
	detailsFn  func(*Bindings) any
	undoFn     func(*Bindings) []UndoEntry
	staticUndo *UndoEntry
}

// Node is the public type alias for plan nodes. Callers receive
// values from the constructor functions but cannot construct the
// underlying struct directly because its fields are unexported.
type Node = node

// Validate creates a pure-check node. Validate nodes have no undo
// capability; if they fail, the operation fails immediately.
func Validate(label string, kind outcome.StepKind, target string,
	fn func(context.Context, *Bindings) error,
) Node {
	return node{
		label:   label,
		flavour: flavourValidate,
		kind:    kind,
		target:  target,
		execFn:  fn,
	}
}

// Produce creates a value-producing node. The returned value is stored
// under the given key and can be retrieved by later nodes via Get.
func Produce[T any](key Key[T], kind outcome.StepKind, target string,
	fn func(context.Context, *Bindings) (T, error),
	opts ...NodeOpt,
) Node {
	n := node{
		label:   key.name,
		flavour: flavourProduce,
		kind:    kind,
		target:  target,
		bindKey: key.name,
		produceFn: func(ctx context.Context, b *Bindings) (any, error) {
			return fn(ctx, b)
		},
	}
	for _, o := range opts {
		o.applyNodeOpt(&n)
	}
	return n
}

// Do creates a side-effecting node. Do nodes support undo via
// WithUndo or UndoFrom options.
func Do(label string, kind outcome.StepKind, target string,
	fn func(context.Context, *Bindings) error,
	opts ...NodeOpt,
) Node {
	n := node{
		label:   label,
		flavour: flavourDo,
		kind:    kind,
		target:  target,
		execFn:  fn,
	}
	for _, o := range opts {
		o.applyNodeOpt(&n)
	}
	return n
}

// Try creates a best-effort node. If the function returns an error, a
// WarnStep is recorded but the operation continues without setting the
// error state. Try nodes have no undo.
func Try(label string, kind outcome.StepKind, target string,
	fn func(context.Context, *Bindings) error,
) Node {
	return node{
		label:   label,
		flavour: flavourTry,
		kind:    kind,
		target:  target,
		execFn:  fn,
	}
}

// NodeOpt configures optional behaviour on Produce and Do nodes.
type NodeOpt interface {
	applyNodeOpt(*node)
}

type nodeOptFunc func(*node)

func (f nodeOptFunc) applyNodeOpt(n *node) { f(n) }

// DetailsFn attaches details to the recorded step after successful
// execution. The closure is called only on success, after the binding
// (if any) has been stored.
func DetailsFn(fn func(*Bindings) any) NodeOpt {
	return nodeOptFunc(func(n *node) {
		n.detailsFn = fn
	})
}

// UndoFrom declares late-bind undo: the closure is called after
// successful execution to compute undo entries from current bindings.
func UndoFrom(fn func(*Bindings) []UndoEntry) NodeOpt {
	return nodeOptFunc(func(n *node) {
		n.undoFn = fn
	})
}

// WithUndo declares a static undo entry known at plan construction
// time.
func WithUndo(entry UndoEntry) NodeOpt {
	return nodeOptFunc(func(n *node) {
		n.staticUndo = &entry
	})
}

// Plan is an ordered list of nodes describing a complete operation.
type Plan struct{ nodes []node }

// Build constructs a Plan from the given nodes.
func Build(nodes ...Node) Plan { return Plan{nodes: nodes} }
