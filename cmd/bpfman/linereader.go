package main

import (
	"bufio"
	"errors"
	"io"
)

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

// scannerReader wraps a bufio.Scanner to implement LineReader for
// non-interactive input (files, pipes).
type scannerReader struct {
	scanner *bufio.Scanner
	closer  io.Closer
}

// NewScannerReader creates a LineReader that reads lines from r. If
// closer is non-nil it is closed when the reader is closed; pass nil
// when reading from os.Stdin to avoid closing it.
func NewScannerReader(r io.Reader, closer io.Closer) LineReader {
	return &scannerReader{
		scanner: bufio.NewScanner(r),
		closer:  closer,
	}
}

func (s *scannerReader) Readline() (string, error) {
	if s.scanner.Scan() {
		return s.scanner.Text(), nil
	}
	if err := s.scanner.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

func (s *scannerReader) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}
