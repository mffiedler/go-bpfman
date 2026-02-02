package outcome_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/frobware/go-bpfman/outcome"
)

func TestSystemState(t *testing.T) {
	tests := []struct {
		name     string
		outcome  outcome.ManagerOperationOutcome
		expected string
	}{
		{
			name:     "success is clean by contract",
			outcome:  outcome.ManagerOperationOutcome{Status: outcome.StatusSuccess},
			expected: "clean",
		},
		{
			name: "failure with no observed residue is clean",
			outcome: outcome.ManagerOperationOutcome{
				Status:   outcome.StatusFailure,
				Observed: nil,
			},
			expected: "clean",
		},
		{
			name: "failure with empty observed is clean",
			outcome: outcome.ManagerOperationOutcome{
				Status:   outcome.StatusFailure,
				Observed: []outcome.Artefact{},
			},
			expected: "clean",
		},
		{
			name: "failure with observed residue is inconsistent",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				},
			},
			expected: "inconsistent",
		},
		{
			name: "failure with observation error is unknown",
			outcome: outcome.ManagerOperationOutcome{
				Status:        outcome.StatusFailure,
				ObservedError: "failed to probe state",
			},
			expected: "unknown",
		},
		{
			name: "observation error takes precedence over observed",
			outcome: outcome.ManagerOperationOutcome{
				Status:        outcome.StatusFailure,
				ObservedError: "probe failed",
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				},
			},
			expected: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.outcome.SystemState()
			if got != tc.expected {
				t.Errorf("SystemState() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestStarted(t *testing.T) {
	tests := []struct {
		name     string
		outcome  outcome.ManagerOperationOutcome
		expected bool
	}{
		{
			name:     "empty outcome not started",
			outcome:  outcome.ManagerOperationOutcome{},
			expected: false,
		},
		{
			name: "with completed steps is started",
			outcome: outcome.ManagerOperationOutcome{
				Completed: []outcome.Step{{Kind: outcome.StepKindKernelLoad}},
			},
			expected: true,
		},
		{
			name: "with failed step is started",
			outcome: outcome.ManagerOperationOutcome{
				Failed: &outcome.Step{Kind: outcome.StepKindKernelLoad},
			},
			expected: true,
		},
		{
			name: "with skipped steps is started",
			outcome: outcome.ManagerOperationOutcome{
				Skipped: []outcome.Step{{Kind: outcome.StepKindKernelLoad}},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.outcome.Started()
			if got != tc.expected {
				t.Errorf("Started() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestNeedsManualCleanup(t *testing.T) {
	tests := []struct {
		name     string
		outcome  outcome.ManagerOperationOutcome
		expected bool
	}{
		{
			name:     "success never needs cleanup",
			outcome:  outcome.ManagerOperationOutcome{Status: outcome.StatusSuccess},
			expected: false,
		},
		{
			name: "success with observed (impossible state) still no cleanup",
			outcome: outcome.ManagerOperationOutcome{
				Status:   outcome.StatusSuccess,
				Observed: []outcome.Artefact{{Kind: outcome.ArtefactProgramPin}},
			},
			expected: false,
		},
		{
			name: "failure without residue needs no cleanup",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
			},
			expected: false,
		},
		{
			name: "failure with residue needs cleanup",
			outcome: outcome.ManagerOperationOutcome{
				Status:   outcome.StatusFailure,
				Observed: []outcome.Artefact{{Kind: outcome.ArtefactProgramPin}},
			},
			expected: true,
		},
		{
			name: "failure with observation error needs cleanup (verification)",
			outcome: outcome.ManagerOperationOutcome{
				Status:        outcome.StatusFailure,
				ObservedError: "probe failed",
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.outcome.NeedsManualCleanup()
			if got != tc.expected {
				t.Errorf("NeedsManualCleanup() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestManualCleanupCommands(t *testing.T) {
	tests := []struct {
		name     string
		outcome  outcome.ManagerOperationOutcome
		expected [][]string
	}{
		{
			name:     "clean state returns nil",
			outcome:  outcome.ManagerOperationOutcome{Status: outcome.StatusSuccess},
			expected: nil,
		},
		{
			name: "unknown state returns nil",
			outcome: outcome.ManagerOperationOutcome{
				Status:        outcome.StatusFailure,
				ObservedError: "probe failed",
			},
			expected: nil,
		},
		{
			name: "program_pin with kernel_id returns unload command",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				},
			},
			expected: [][]string{{"bpfman", "unload", "123"}},
		},
		{
			name: "link_pin with link_id returns detach command",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactLinkPin, LinkID: 456},
				},
			},
			expected: [][]string{{"bpfman", "detach", "--id", "456"}},
		},
		{
			name: "deduplicates same kernel_id",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				},
			},
			expected: [][]string{{"bpfman", "unload", "123"}},
		},
		{
			name: "suppresses maps_dir for same kernel_id",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
					{Kind: outcome.ArtefactMapsDir, KernelID: 123, Path: "/sys/fs/bpf/bpfman/123/maps"},
				},
			},
			expected: [][]string{{"bpfman", "unload", "123"}},
		},
		{
			name: "maps_dir without kernel_id triggers gc",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactMapsDir, Path: "/sys/fs/bpf/bpfman/orphan/maps"},
				},
			},
			expected: [][]string{{"bpfman", "gc"}},
		},
		{
			name: "dispatcher triggers gc",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactDispatcher, KernelID: 789},
				},
			},
			expected: [][]string{{"bpfman", "gc"}},
		},
		{
			name: "mixed artefacts returns multiple commands",
			outcome: outcome.ManagerOperationOutcome{
				Status: outcome.StatusFailure,
				Observed: []outcome.Artefact{
					{Kind: outcome.ArtefactProgramPin, KernelID: 123},
					{Kind: outcome.ArtefactLinkPin, LinkID: 456},
					{Kind: outcome.ArtefactDispatcher, KernelID: 789},
				},
			},
			expected: [][]string{
				{"bpfman", "unload", "123"},
				{"bpfman", "detach", "--id", "456"},
				{"bpfman", "gc"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.outcome.ManualCleanupCommands()
			if !equalCmds(got, tc.expected) {
				t.Errorf("ManualCleanupCommands() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func equalCmds(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func TestRecorder_Complete(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	step := outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "test_prog"}
	if err := rec.Complete(step); err != nil {
		t.Fatalf("Complete() failed: %v", err)
	}

	if len(out.Completed) != 1 {
		t.Errorf("Completed has %d steps, want 1", len(out.Completed))
	}
	if out.Completed[0].Target != "test_prog" {
		t.Errorf("Completed[0].Target = %q, want %q", out.Completed[0].Target, "test_prog")
	}
}

func TestRecorder_CompleteAfterFailReturnsError(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	failStep := outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "failing", Error: "boom"}
	if err := rec.Fail(failStep); err != nil {
		t.Fatalf("Fail() failed: %v", err)
	}

	step := outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "another"}
	err := rec.Complete(step)
	if !errors.Is(err, outcome.ErrAlreadyFailed) {
		t.Errorf("Complete() after Fail() returned %v, want ErrAlreadyFailed", err)
	}
}

func TestRecorder_Fail(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	step := outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "test_prog", Error: "load failed"}
	if err := rec.Fail(step); err != nil {
		t.Fatalf("Fail() failed: %v", err)
	}

	if out.Status != outcome.StatusFailure {
		t.Errorf("Status = %q, want %q", out.Status, outcome.StatusFailure)
	}
	if out.Failed == nil {
		t.Fatal("Failed is nil")
	}
	if out.Failed.Target != "test_prog" {
		t.Errorf("Failed.Target = %q, want %q", out.Failed.Target, "test_prog")
	}
}

func TestRecorder_DoubleFailReturnsError(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	step1 := outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "first"}
	if err := rec.Fail(step1); err != nil {
		t.Fatalf("first Fail() failed: %v", err)
	}

	step2 := outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "second"}
	err := rec.Fail(step2)
	if !errors.Is(err, outcome.ErrAlreadyFailed) {
		t.Errorf("second Fail() returned %v, want ErrAlreadyFailed", err)
	}
}

func TestRecorder_Cleanup(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	// Fail first
	failStep := outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "load failed"}
	_ = rec.Fail(failStep)

	// Begin cleanup
	rec.BeginCleanup()

	// Record cleanup step
	cleanupStep := outcome.Step{Kind: outcome.StepKindKernelUnload, Target: "test_prog"}
	if err := rec.CleanupComplete(cleanupStep); err != nil {
		t.Fatalf("CleanupComplete() failed: %v", err)
	}

	if out.Cleanup == nil {
		t.Fatal("Cleanup is nil")
	}
	if out.Cleanup.Status != outcome.StatusSuccess {
		t.Errorf("Cleanup.Status = %q, want %q", out.Cleanup.Status, outcome.StatusSuccess)
	}
	if len(out.Cleanup.Completed) != 1 {
		t.Errorf("Cleanup.Completed has %d steps, want 1", len(out.Cleanup.Completed))
	}
}

func TestRecorder_CleanupFailFlipsStatus(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "load failed"})
	rec.BeginCleanup()

	failedCleanup := outcome.Step{Kind: outcome.StepKindKernelUnload, Error: "permission denied"}
	if err := rec.CleanupFail(failedCleanup); err != nil {
		t.Fatalf("CleanupFail() failed: %v", err)
	}

	if out.Cleanup.Status != outcome.StatusFailure {
		t.Errorf("Cleanup.Status = %q, want %q", out.Cleanup.Status, outcome.StatusFailure)
	}
	if len(out.Cleanup.Failed) != 1 {
		t.Errorf("Cleanup.Failed has %d steps, want 1", len(out.Cleanup.Failed))
	}
}

func TestRecorder_CleanupWithoutBeginReturnsError(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	step := outcome.Step{Kind: outcome.StepKindKernelUnload}
	err := rec.CleanupComplete(step)
	if !errors.Is(err, outcome.ErrCleanupNotActive) {
		t.Errorf("CleanupComplete() without BeginCleanup() returned %v, want ErrCleanupNotActive", err)
	}

	err = rec.CleanupFail(step)
	if !errors.Is(err, outcome.ErrCleanupNotActive) {
		t.Errorf("CleanupFail() without BeginCleanup() returned %v, want ErrCleanupNotActive", err)
	}
}

func TestRecorder_SetObserved(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	artefacts := []outcome.Artefact{
		{Kind: outcome.ArtefactProgramPin, KernelID: 123},
	}
	rec.SetObserved(artefacts, nil)

	if len(out.Observed) != 1 {
		t.Errorf("Observed has %d items, want 1", len(out.Observed))
	}
	if out.ObservedError != "" {
		t.Errorf("ObservedError = %q, want empty", out.ObservedError)
	}
}

func TestRecorder_SetObservedWithError(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	observeErr := errors.New("failed to probe")
	rec.SetObserved(nil, observeErr)

	if out.ObservedError != "failed to probe" {
		t.Errorf("ObservedError = %q, want %q", out.ObservedError, "failed to probe")
	}
}

func TestRecorder_Validate_Success(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Complete(outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "prog"})

	if err := rec.Validate(); err != nil {
		t.Errorf("Validate() failed: %v", err)
	}
}

func TestRecorder_Validate_FailureWithFailedStep(t *testing.T) {
	var out outcome.ManagerOperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "boom"})
	out.Error = "load failed: boom"

	if err := rec.Validate(); err != nil {
		t.Errorf("Validate() failed: %v", err)
	}
}

func TestRecorder_Validate_FailureWithoutFailedStepOrError(t *testing.T) {
	out := outcome.ManagerOperationOutcome{Status: outcome.StatusFailure}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for failure without failed step or error")
	}
}

func TestRecorder_Validate_SuccessWithFailedStep(t *testing.T) {
	out := outcome.ManagerOperationOutcome{
		Status: outcome.StatusSuccess,
		Failed: &outcome.Step{Kind: outcome.StepKindKernelLoad},
	}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for success with failed step")
	}
}

func TestRecorder_Validate_CleanupSuccessWithFailedSteps(t *testing.T) {
	out := outcome.ManagerOperationOutcome{
		Status: outcome.StatusFailure,
		Failed: &outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "boom"},
		Error:  "boom",
		Cleanup: &outcome.CleanupOutcome{
			Status: outcome.StatusSuccess,
			Failed: []outcome.Step{{Kind: outcome.StepKindKernelUnload}},
		},
	}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for cleanup success with failed steps")
	}
}

func TestRecorder_Validate_NonJSONSafeDetails(t *testing.T) {
	out := outcome.ManagerOperationOutcome{
		Status: outcome.StatusSuccess,
		Completed: []outcome.Step{
			{
				Kind:    outcome.StepKindKernelLoad,
				Details: make(chan int), // channels are not JSON-safe
			},
		},
	}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for non-JSON-safe details")
	}
}

func TestFailFromErr(t *testing.T) {
	err := errors.New("something broke")
	step := outcome.FailFromErr(outcome.StepKindKernelLoad, "my_prog", err)

	if step.Kind != outcome.StepKindKernelLoad {
		t.Errorf("Kind = %q, want %q", step.Kind, outcome.StepKindKernelLoad)
	}
	if step.Target != "my_prog" {
		t.Errorf("Target = %q, want %q", step.Target, "my_prog")
	}
	if step.Error != "something broke" {
		t.Errorf("Error = %q, want %q", step.Error, "something broke")
	}
}

func TestOutcomeJSONSerialization(t *testing.T) {
	out := outcome.ManagerOperationOutcome{
		OpID:   42,
		Status: outcome.StatusFailure,
		Error:  "load failed",
		Completed: []outcome.Step{
			{
				Kind:    outcome.StepKindKernelLoad,
				Target:  "prog_a",
				Details: outcome.ProgramDetails{KernelID: 123},
			},
		},
		Failed: &outcome.Step{
			Kind:   outcome.StepKindKernelLoad,
			Target: "prog_b",
			Error:  "invalid BTF",
		},
		Skipped: []outcome.Step{
			{Kind: outcome.StepKindKernelLoad, Target: "prog_c"},
		},
		Cleanup: &outcome.CleanupOutcome{
			Status: outcome.StatusSuccess,
			Completed: []outcome.Step{
				{
					Kind:    outcome.StepKindKernelUnload,
					Target:  "prog_a",
					Details: outcome.ProgramDetails{KernelID: 123},
				},
			},
		},
		Observed: []outcome.Artefact{},
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	var decoded outcome.ManagerOperationOutcome
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() failed: %v", err)
	}

	if decoded.OpID != 42 {
		t.Errorf("OpID = %d, want 42", decoded.OpID)
	}
	if decoded.Status != outcome.StatusFailure {
		t.Errorf("Status = %q, want %q", decoded.Status, outcome.StatusFailure)
	}
	if len(decoded.Completed) != 1 {
		t.Errorf("Completed has %d items, want 1", len(decoded.Completed))
	}
	if decoded.Failed == nil {
		t.Error("Failed is nil")
	}
	if len(decoded.Skipped) != 1 {
		t.Errorf("Skipped has %d items, want 1", len(decoded.Skipped))
	}
	if decoded.Cleanup == nil {
		t.Error("Cleanup is nil")
	}
}

func TestArtefactString(t *testing.T) {
	tests := []struct {
		artefact outcome.Artefact
		expected string
	}{
		{
			artefact: outcome.Artefact{Kind: outcome.ArtefactProgramPin, KernelID: 123, Path: "/sys/fs/bpf/prog"},
			expected: "program_pin(kernel_id=123, path=/sys/fs/bpf/prog)",
		},
		{
			artefact: outcome.Artefact{Kind: outcome.ArtefactProgramPin, Path: "/sys/fs/bpf/prog"},
			expected: "program_pin(path=/sys/fs/bpf/prog)",
		},
		{
			artefact: outcome.Artefact{Kind: outcome.ArtefactLinkPin, LinkID: 456, Path: "/sys/fs/bpf/link"},
			expected: "link_pin(link_id=456, path=/sys/fs/bpf/link)",
		},
		{
			artefact: outcome.Artefact{Kind: outcome.ArtefactMapsDir, Path: "/sys/fs/bpf/maps"},
			expected: "maps_dir(path=/sys/fs/bpf/maps)",
		},
		{
			artefact: outcome.Artefact{Kind: outcome.ArtefactDispatcher, KernelID: 789},
			expected: "dispatcher(kernel_id=789)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			got := tc.artefact.String()
			if got != tc.expected {
				t.Errorf("String() = %q, want %q", got, tc.expected)
			}
		})
	}
}
