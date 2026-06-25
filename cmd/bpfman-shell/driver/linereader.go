package driver

import (
	"bufio"
	"errors"
	"io"
)

// ErrInterrupt is returned when the user presses Ctrl-C.
var ErrInterrupt = errors.New("interrupted")

// LineReader provides line-oriented input for whole-program file
// and stdin execution.
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
