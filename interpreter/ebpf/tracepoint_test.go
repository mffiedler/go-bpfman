package ebpf

import (
	"strings"
	"testing"
)

func TestTracepointExists(t *testing.T) {
	// sched/sched_switch is a standard tracepoint that should always exist
	if !tracepointExists("sched", "sched_switch") {
		t.Skip("sched/sched_switch tracepoint not available (running in container?)")
	}

	tests := []struct {
		name   string
		group  string
		tp     string
		exists bool
	}{
		{"valid tracepoint", "sched", "sched_switch", true},
		{"invalid tracepoint name", "sched", "nonexistent_tracepoint_xyz", false},
		{"invalid group", "nonexistent_group_xyz", "anything", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tracepointExists(tt.group, tt.tp)
			if got != tt.exists {
				t.Errorf("tracepointExists(%q, %q) = %v, want %v", tt.group, tt.tp, got, tt.exists)
			}
		})
	}
}

func TestValidateTracepoint(t *testing.T) {
	if !tracepointExists("sched", "sched_switch") {
		t.Skip("sched/sched_switch tracepoint not available (running in container?)")
	}

	t.Run("valid tracepoint", func(t *testing.T) {
		err := validateTracepoint("sched", "sched_switch")
		if err != nil {
			t.Errorf("unexpected error for valid tracepoint: %v", err)
		}
	})

	t.Run("invalid tracepoint", func(t *testing.T) {
		err := validateTracepoint("sched", "nonexistent_xyz")
		if err == nil {
			t.Fatal("expected error for invalid tracepoint")
		}

		tpErr, ok := err.(ErrTracepointNotFound)
		if !ok {
			t.Fatalf("expected ErrTracepointNotFound, got %T", err)
		}

		if tpErr.Group != "sched" {
			t.Errorf("expected group 'sched', got %q", tpErr.Group)
		}
		if tpErr.Name != "nonexistent_xyz" {
			t.Errorf("expected name 'nonexistent_xyz', got %q", tpErr.Name)
		}

		// Check error message format
		errMsg := err.Error()
		if !strings.Contains(errMsg, "tracepoint 'sched/nonexistent_xyz' does not exist") {
			t.Errorf("error message missing tracepoint info: %s", errMsg)
		}
		if !strings.Contains(errMsg, "/sys/kernel/tracing/events/sched") {
			t.Errorf("error message missing path hint: %s", errMsg)
		}
	})

	t.Run("invalid group", func(t *testing.T) {
		err := validateTracepoint("nonexistent_group_xyz", "anything")
		if err == nil {
			t.Fatal("expected error for invalid group")
		}

		_, ok := err.(ErrTracepointNotFound)
		if !ok {
			t.Fatalf("expected ErrTracepointNotFound, got %T", err)
		}
	})
}

func TestErrTracepointNotFound_Error(t *testing.T) {
	err := ErrTracepointNotFound{
		Group: "foo",
		Name:  "bar",
	}

	msg := err.Error()
	if !strings.Contains(msg, "tracepoint 'foo/bar' does not exist") {
		t.Errorf("error message missing tracepoint info: %s", msg)
	}
	if !strings.Contains(msg, "/sys/kernel/tracing/events/foo") {
		t.Errorf("error message missing path: %s", msg)
	}
}
