package ebpf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// tracingEventsPath is the base path for tracepoint events.
	tracingEventsPath = "/sys/kernel/tracing/events"
)

// ErrTracepointNotFound indicates a tracepoint does not exist.
type ErrTracepointNotFound struct {
	Group string
	Name  string
}

func (e ErrTracepointNotFound) Error() string {
	groupPath := filepath.Join(tracingEventsPath, e.Group)
	return fmt.Sprintf("tracepoint '%s/%s' does not exist; see %s for available tracepoints", e.Group, e.Name, groupPath)
}

// tracepointExists checks if a tracepoint exists in tracefs.
func tracepointExists(group, name string) bool {
	idPath := filepath.Join(tracingEventsPath, group, name, "id")
	_, err := os.Stat(idPath)
	return err == nil
}

// validateTracepoint checks if a tracepoint exists and returns a helpful
// error if not.
func validateTracepoint(group, name string) error {
	if tracepointExists(group, name) {
		return nil
	}
	return ErrTracepointNotFound{Group: group, Name: name}
}

// isTracepointNotFoundError checks if an error indicates a missing tracepoint.
// This detects the low-level error from cilium/ebpf when the tracefs file
// doesn't exist.
func isTracepointNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	// The error from cilium/ebpf contains "no such file or directory"
	// when the tracepoint doesn't exist
	return errors.Is(err, os.ErrNotExist) ||
		strings.Contains(err.Error(), "no such file or directory")
}
