package main

import (
	"errors"
)

// ErrSilent is returned when the error has already been communicated
// (e.g., via JSON output) and cli.go should exit non-zero without
// printing an additional error message.
var ErrSilent = errors.New("silent error")
