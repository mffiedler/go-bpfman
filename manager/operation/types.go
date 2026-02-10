package operation

import (
	"fmt"
)

// Key is a typed reference to a plan binding. The type parameter
// ensures that Get returns the correct type without requiring callers
// to assert.
type Key[T any] struct{ name string }

// NewKey creates a binding key with the given name.
func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

// Bindings stores values produced by Produce nodes during execution.
type Bindings struct{ m map[string]any }

func newBindings() *Bindings { return &Bindings{m: make(map[string]any)} }

// Get retrieves a typed value from bindings. Panics if the key is
// absent; this is a programming error indicating the Produce node was
// skipped or has not run yet.
func Get[T any](b *Bindings, key Key[T]) T {
	v, ok := b.m[key.name]
	if !ok {
		panic(fmt.Sprintf("operation.Get: key %q not bound", key.name))
	}
	val, ok2 := v.(T)
	if !ok2 {
		panic(fmt.Sprintf("operation.Get: key %q has type %T, not %T", key.name, v, val))
	}
	return val
}
