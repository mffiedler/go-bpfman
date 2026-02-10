package operation

import (
	"fmt"
	"reflect"
)

// registry tracks all key names to detect duplicate registrations at
// startup. The map value records the concrete type so the panic
// message can report what collided.
var registry = map[string]reflect.Type{}

// Key is a typed reference to a plan binding. The type parameter
// ensures that Get returns the correct type without requiring callers
// to assert.
type Key[T any] struct{ name string }

// NewKey creates a binding key with the given name. It panics if a
// key with the same name has already been registered, catching
// accidental name collisions at process startup rather than during
// operation execution.
func NewKey[T any](name string) Key[T] {
	t := reflect.TypeFor[T]()
	if existing, ok := registry[name]; ok {
		panic(fmt.Sprintf("operation.NewKey: %q already registered with type %s (got %s)", name, existing, t))
	}
	registry[name] = t
	return Key[T]{name: name}
}

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
