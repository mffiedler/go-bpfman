package operation

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/manager/action"
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
	target  string

	// Exactly one of these is set, depending on flavour.
	// validate/do/try use execFn; produce uses produceFn.
	execFn    func(context.Context, action.ExecutorWithResult, *Bindings) error
	produceFn func(context.Context, action.ExecutorWithResult, *Bindings) (any, error)

	// For produce: the key name used to store the binding.
	bindKey string

	// Options (only meaningful for produce/do).
	undoFn     func(*Bindings) []action.Action
	staticUndo []action.Action
}

// Node is the public type alias for plan nodes. Callers receive
// values from the constructor functions but cannot construct the
// underlying struct directly because its fields are unexported.
type Node = node

// Validate creates a pure-check node. Validate nodes have no undo
// capability; if they fail, the operation fails immediately.
//
// The closure signature omits the executor because validation is
// semantically pure (no I/O). The narrower type enforces that
// constraint at compile time.
func Validate(label string, target string,
	fn func(context.Context, *Bindings) error,
) Node {
	return node{
		label:   label,
		flavour: flavourValidate,
		target:  target,
		execFn: func(ctx context.Context, _ action.ExecutorWithResult, b *Bindings) error {
			return fn(ctx, b)
		},
	}
}

// Produce creates a value-producing node. The returned value is stored
// under the given key and can be retrieved by later nodes via Get.
func Produce[T any](key Key[T], target string,
	fn func(context.Context, action.ExecutorWithResult, *Bindings) (T, error),
	opts ...NodeOpt,
) Node {
	n := node{
		label:   key.name,
		flavour: flavourProduce,
		target:  target,
		bindKey: key.name,
		produceFn: func(ctx context.Context, exec action.ExecutorWithResult, b *Bindings) (any, error) {
			return fn(ctx, exec, b)
		},
	}
	for _, o := range opts {
		o.applyNodeOpt(&n)
	}
	return n
}

// Do creates a side-effecting node. Do nodes support undo via
// WithUndo or UndoFrom options.
func Do(label string, target string,
	fn func(context.Context, action.ExecutorWithResult, *Bindings) error,
	opts ...NodeOpt,
) Node {
	n := node{
		label:   label,
		flavour: flavourDo,
		target:  target,
		execFn:  fn,
	}
	for _, o := range opts {
		o.applyNodeOpt(&n)
	}
	return n
}

// Try creates a best-effort node. If the function returns an error,
// the operation continues without setting the error state. Try nodes
// have no undo.
func Try(label string, target string,
	fn func(context.Context, action.ExecutorWithResult, *Bindings) error,
) Node {
	return node{
		label:   label,
		flavour: flavourTry,
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

// UndoFrom declares late-bind undo: the closure is called after
// successful execution to compute undo actions from current bindings.
func UndoFrom(fn func(*Bindings) []action.Action) NodeOpt {
	return nodeOptFunc(func(n *node) {
		n.undoFn = fn
	})
}

// WithUndo declares static undo actions known at plan construction
// time.
func WithUndo(actions ...action.Action) NodeOpt {
	return nodeOptFunc(func(n *node) {
		n.staticUndo = actions
	})
}

// Plan is an ordered list of nodes describing a complete operation.
type Plan struct{ nodes []node }

// Build constructs a Plan from the given nodes. It panics if two
// Produce nodes bind the same key, catching the error at plan
// construction time rather than during execution.
func Build(nodes ...Node) Plan {
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.flavour == flavourProduce {
			if seen[n.bindKey] {
				panic(fmt.Sprintf("operation.Build: duplicate Produce key %q", n.bindKey))
			}
			seen[n.bindKey] = true
		}
	}
	return Plan{nodes: nodes}
}
