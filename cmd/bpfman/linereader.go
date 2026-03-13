package main

import "errors"

// ErrInterrupt is returned when the user presses Ctrl-C.
var ErrInterrupt = errors.New("interrupted")

// CompleteFunc computes completions for line at rune position pos.
// Returns how many runes before pos to replace, and the full
// replacement candidates.
type CompleteFunc func(line string, pos int) (replace int, candidates []string)

// LineReader provides line-editing, history, and completion for an
// interactive prompt. The interface is library-agnostic so the
// backing implementation can be swapped without touching callers.
type LineReader interface {
	Readline() (string, error)
	Close() error
}
