// Package store provides storage interfaces and errors.
package store

import "errors"

// ErrNotFound is returned when a requested item does not exist in the store.
var ErrNotFound = errors.New("not found")
