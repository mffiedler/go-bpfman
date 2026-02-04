package outcome_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/frobware/go-bpfman/outcome"
)

func TestComputeSystemState(t *testing.T) {
	tests := []struct {
		name          string
		status        outcome.Status
		residual      []outcome.Artefact
		residualError string
		expected      string
	}{
		{
			name:     "success is clean by contract",
			status:   outcome.StatusSuccess,
			expected: "clean",
		},
		{
			name:     "failure with no residue is clean",
			status:   outcome.StatusFailure,
			residual: nil,
			expected: "clean",
		},
		{
			name:     "failure with empty residue is clean",
			status:   outcome.StatusFailure,
			residual: []outcome.Artefact{},
			expected: "clean",
		},
		{
			name:   "failure with residue is inconsistent",
			status: outcome.StatusFailure,
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
			},
			expected: "inconsistent",
		},
		{
			name:          "failure with residual error is unknown",
			status:        outcome.StatusFailure,
			residualError: "failed to probe state",
			expected:      "unknown",
		},
		{
			name:          "residual error takes precedence over residue",
			status:        outcome.StatusFailure,
			residualError: "probe failed",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
			},
			expected: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := outcome.ComputeSystemState(tc.status, tc.residual, tc.residualError)
			if got != tc.expected {
				t.Errorf("ComputeSystemState() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestStarted(t *testing.T) {
	tests := []struct {
		name     string
		timeline []outcome.TimelineEntry
		expected bool
	}{
		{
			name:     "empty timeline not started",
			timeline: nil,
			expected: false,
		},
		{
			name: "with completed entry is started",
			timeline: []outcome.TimelineEntry{
				{Seq: 1, Phase: outcome.PhasePrimary, Status: outcome.StepStatusCompleted, Kind: outcome.StepKindKernelLoad},
			},
			expected: true,
		},
		{
			name: "with failed entry is started",
			timeline: []outcome.TimelineEntry{
				{Seq: 1, Phase: outcome.PhasePrimary, Status: outcome.StepStatusFailed, Kind: outcome.StepKindKernelLoad},
			},
			expected: true,
		},
		{
			name: "with skipped entry is started",
			timeline: []outcome.TimelineEntry{
				{Seq: 1, Phase: outcome.PhasePrimary, Status: outcome.StepStatusSkipped, Kind: outcome.StepKindKernelLoad},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out outcome.OperationOutcome
			out.Timeline = tc.timeline
			rec := outcome.NewRecorder(&out)
			got := rec.Started()
			if got != tc.expected {
				t.Errorf("Started() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestComputeManualCleanupRequired(t *testing.T) {
	tests := []struct {
		name        string
		status      outcome.Status
		systemState string
		expected    bool
	}{
		{
			name:        "success never needs cleanup",
			status:      outcome.StatusSuccess,
			systemState: "clean",
			expected:    false,
		},
		{
			name:        "failure with clean state needs no cleanup",
			status:      outcome.StatusFailure,
			systemState: "clean",
			expected:    false,
		},
		{
			name:        "failure with inconsistent state needs cleanup",
			status:      outcome.StatusFailure,
			systemState: "inconsistent",
			expected:    true,
		},
		{
			name:        "failure with unknown state needs cleanup (verification)",
			status:      outcome.StatusFailure,
			systemState: "unknown",
			expected:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := outcome.ComputeManualCleanupRequired(tc.status, tc.systemState)
			if got != tc.expected {
				t.Errorf("ComputeManualCleanupRequired() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestComputeManualCleanupCommands(t *testing.T) {
	tests := []struct {
		name        string
		systemState string
		residual    []outcome.Artefact
		expected    [][]string
	}{
		{
			name:        "clean state returns nil",
			systemState: "clean",
			expected:    nil,
		},
		{
			name:        "unknown state returns nil",
			systemState: "unknown",
			expected:    nil,
		},
		{
			name:        "program_pin with kernel_id returns unload command",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
			},
			expected: [][]string{{"bpfman", "unload", "123"}},
		},
		{
			name:        "link_pin with link_id returns detach command",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactLinkPin, LinkID: 456},
			},
			expected: [][]string{{"bpfman", "detach", "--id", "456"}},
		},
		{
			name:        "deduplicates same kernel_id",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
			},
			expected: [][]string{{"bpfman", "unload", "123"}},
		},
		{
			name:        "suppresses maps_dir for same kernel_id",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				{Kind: outcome.ArtefactMapsDir, KernelID: 123, Path: "/sys/fs/bpf/bpfman/123/maps"},
			},
			expected: [][]string{{"bpfman", "unload", "123"}},
		},
		{
			name:        "maps_dir without kernel_id triggers gc",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactMapsDir, Path: "/sys/fs/bpf/bpfman/orphan/maps"},
			},
			expected: [][]string{{"bpfman", "gc"}},
		},
		{
			name:        "dispatcher triggers gc",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactDispatcher, KernelID: 789},
			},
			expected: [][]string{{"bpfman", "gc"}},
		},
		{
			name:        "mixed artefacts returns multiple commands",
			systemState: "inconsistent",
			residual: []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: 123},
				{Kind: outcome.ArtefactLinkPin, LinkID: 456},
				{Kind: outcome.ArtefactDispatcher, KernelID: 789},
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
			got := outcome.ComputeManualCleanupCommands(tc.systemState, tc.residual)
			if !equalCmds(got, tc.expected) {
				t.Errorf("ComputeManualCleanupCommands() = %v, want %v", got, tc.expected)
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
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	step := outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "test_prog"}
	if err := rec.Complete(step); err != nil {
		t.Fatalf("Complete() failed: %v", err)
	}

	if len(out.Timeline) != 1 {
		t.Errorf("Timeline has %d entries, want 1", len(out.Timeline))
	}
	if out.Timeline[0].Target != "test_prog" {
		t.Errorf("Timeline[0].Target = %q, want %q", out.Timeline[0].Target, "test_prog")
	}
	if out.Timeline[0].Seq != 1 {
		t.Errorf("Timeline[0].Seq = %d, want 1", out.Timeline[0].Seq)
	}
	if out.Timeline[0].Phase != outcome.PhasePrimary {
		t.Errorf("Timeline[0].Phase = %q, want %q", out.Timeline[0].Phase, outcome.PhasePrimary)
	}
	if out.Timeline[0].Status != outcome.StepStatusCompleted {
		t.Errorf("Timeline[0].Status = %q, want %q", out.Timeline[0].Status, outcome.StepStatusCompleted)
	}
}

func TestRecorder_CompleteAfterFailReturnsError(t *testing.T) {
	var out outcome.OperationOutcome
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
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	step := outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "test_prog", Error: "load failed"}
	if err := rec.Fail(step); err != nil {
		t.Fatalf("Fail() failed: %v", err)
	}

	if out.Status != outcome.StatusFailure {
		t.Errorf("Status = %q, want %q", out.Status, outcome.StatusFailure)
	}
	if len(out.Timeline) != 1 {
		t.Fatalf("Timeline has %d entries, want 1", len(out.Timeline))
	}
	if out.Timeline[0].Target != "test_prog" {
		t.Errorf("Timeline[0].Target = %q, want %q", out.Timeline[0].Target, "test_prog")
	}
	if out.Timeline[0].Status != outcome.StepStatusFailed {
		t.Errorf("Timeline[0].Status = %q, want %q", out.Timeline[0].Status, outcome.StepStatusFailed)
	}
	if out.Timeline[0].Error != "load failed" {
		t.Errorf("Timeline[0].Error = %q, want %q", out.Timeline[0].Error, "load failed")
	}
}

func TestRecorder_DoubleFailReturnsError(t *testing.T) {
	var out outcome.OperationOutcome
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

func TestRecorder_Rollback(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	// Complete a step first
	_ = rec.Complete(outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "test_prog"})

	// Fail
	failStep := outcome.Step{Kind: outcome.StepKindStoreSaveProgram, Error: "db failed"}
	_ = rec.Fail(failStep)

	// Begin rollback
	rec.BeginRollback()

	// Record rollback step
	rollbackStep := outcome.Step{Kind: outcome.StepKindKernelUnload, Target: "test_prog"}
	if err := rec.RollbackComplete(rollbackStep); err != nil {
		t.Fatalf("RollbackComplete() failed: %v", err)
	}

	if len(out.Timeline) != 3 {
		t.Fatalf("Timeline has %d entries, want 3", len(out.Timeline))
	}

	// Check rollback entry
	rollbackEntry := out.Timeline[2]
	if rollbackEntry.Phase != outcome.PhaseRollback {
		t.Errorf("Timeline[2].Phase = %q, want %q", rollbackEntry.Phase, outcome.PhaseRollback)
	}
	if rollbackEntry.Status != outcome.StepStatusCompleted {
		t.Errorf("Timeline[2].Status = %q, want %q", rollbackEntry.Status, outcome.StepStatusCompleted)
	}
	if rollbackEntry.Target != "test_prog" {
		t.Errorf("Timeline[2].Target = %q, want %q", rollbackEntry.Target, "test_prog")
	}
}

func TestRecorder_RollbackFailFlipsStatus(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "load failed"})
	rec.BeginRollback()

	failedRollback := outcome.Step{Kind: outcome.StepKindKernelUnload, Error: "permission denied"}
	if err := rec.RollbackFail(failedRollback); err != nil {
		t.Fatalf("RollbackFail() failed: %v", err)
	}

	if !rec.RollbackFailed() {
		t.Error("RollbackFailed() = false, want true")
	}

	// Check the rollback entry
	rollbackEntry := out.Timeline[1]
	if rollbackEntry.Phase != outcome.PhaseRollback {
		t.Errorf("Timeline[1].Phase = %q, want %q", rollbackEntry.Phase, outcome.PhaseRollback)
	}
	if rollbackEntry.Status != outcome.StepStatusFailed {
		t.Errorf("Timeline[1].Status = %q, want %q", rollbackEntry.Status, outcome.StepStatusFailed)
	}
	if rollbackEntry.Error != "permission denied" {
		t.Errorf("Timeline[1].Error = %q, want %q", rollbackEntry.Error, "permission denied")
	}
}

func TestRecorder_RollbackWithoutBeginReturnsError(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	step := outcome.Step{Kind: outcome.StepKindKernelUnload}
	err := rec.RollbackComplete(step)
	if !errors.Is(err, outcome.ErrRollbackNotActive) {
		t.Errorf("RollbackComplete() without BeginRollback() returned %v, want ErrRollbackNotActive", err)
	}

	err = rec.RollbackFail(step)
	if !errors.Is(err, outcome.ErrRollbackNotActive) {
		t.Errorf("RollbackFail() without BeginRollback() returned %v, want ErrRollbackNotActive", err)
	}
}

func TestRecorder_SetResidual(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	artefacts := []outcome.Artefact{
		{Kind: outcome.ArtefactProgramPin, KernelID: 123},
	}
	rec.SetResidual(artefacts, nil)

	if len(out.Residual) != 1 {
		t.Errorf("Residual has %d items, want 1", len(out.Residual))
	}
	if out.ResidualError != "" {
		t.Errorf("ResidualError = %q, want empty", out.ResidualError)
	}
}

func TestRecorder_SetResidualWithError(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	observeErr := errors.New("failed to probe")
	rec.SetResidual(nil, observeErr)

	if out.ResidualError != "failed to probe" {
		t.Errorf("ResidualError = %q, want %q", out.ResidualError, "failed to probe")
	}
}

func TestRecorder_Finalise(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	// Simulate a failed operation with residue
	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "boom"})
	rec.SetResidual([]outcome.Artefact{
		{Kind: outcome.ArtefactProgramPin, KernelID: 123},
	}, nil)

	rec.Finalise()

	if out.SystemState != "inconsistent" {
		t.Errorf("SystemState = %q, want %q", out.SystemState, "inconsistent")
	}
	if !out.ManualCleanupRequired {
		t.Error("ManualCleanupRequired = false, want true")
	}
	if len(out.ManualCleanupCommands) != 1 {
		t.Errorf("ManualCleanupCommands has %d items, want 1", len(out.ManualCleanupCommands))
	}
}

func TestRecorder_FinaliseCleanState(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	// Simulate a failed operation with successful rollback (no residue)
	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindStoreSaveProgram, Error: "db error"})
	rec.BeginRollback()
	_ = rec.RollbackComplete(outcome.Step{Kind: outcome.StepKindKernelUnload})
	rec.SetResidual(nil, nil)

	rec.Finalise()

	if out.SystemState != "clean" {
		t.Errorf("SystemState = %q, want %q", out.SystemState, "clean")
	}
	if out.ManualCleanupRequired {
		t.Error("ManualCleanupRequired = true, want false")
	}
	if out.ManualCleanupCommands != nil {
		t.Errorf("ManualCleanupCommands = %v, want nil", out.ManualCleanupCommands)
	}
}

func TestRecorder_Validate_Success(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Complete(outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "prog"})

	if err := rec.Validate(); err != nil {
		t.Errorf("Validate() failed: %v", err)
	}
}

func TestRecorder_Validate_FailureWithFailedStep(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, Error: "boom"})
	out.PrimaryError = "load failed: boom"

	if err := rec.Validate(); err != nil {
		t.Errorf("Validate() failed: %v", err)
	}
}

func TestRecorder_Validate_FailureWithoutFailedStepOrError(t *testing.T) {
	out := outcome.OperationOutcome{Status: outcome.StatusFailure}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for failure without failed step or error")
	}
}

func TestRecorder_Validate_SuccessWithFailedStep(t *testing.T) {
	out := outcome.OperationOutcome{
		Status: outcome.StatusSuccess,
		Timeline: []outcome.TimelineEntry{
			{Seq: 1, Phase: outcome.PhasePrimary, Status: outcome.StepStatusFailed, Kind: outcome.StepKindKernelLoad},
		},
	}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for success with failed step")
	}
}

func TestRecorder_Validate_RollbackFailedWithoutErrors(t *testing.T) {
	out := outcome.OperationOutcome{
		Status:       outcome.StatusFailure,
		PrimaryError: "boom",
		Timeline: []outcome.TimelineEntry{
			{Seq: 1, Phase: outcome.PhasePrimary, Status: outcome.StepStatusFailed, Kind: outcome.StepKindKernelLoad, Error: "boom"},
			{Seq: 2, Phase: outcome.PhaseRollback, Status: outcome.StepStatusFailed, Kind: outcome.StepKindKernelUnload, Error: "perm denied"},
		},
		// RollbackErrors not set - this is the invalid state
	}
	rec := outcome.NewRecorder(&out)

	err := rec.Validate()
	if err == nil {
		t.Error("Validate() should fail for rollback failed without rollback errors")
	}
}

func TestRecorder_Validate_NonJSONSafeDetails(t *testing.T) {
	out := outcome.OperationOutcome{
		Status: outcome.StatusSuccess,
		Timeline: []outcome.TimelineEntry{
			{
				Seq:     1,
				Phase:   outcome.PhasePrimary,
				Status:  outcome.StepStatusCompleted,
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
	out := outcome.OperationOutcome{
		OpID:         42,
		Status:       outcome.StatusFailure,
		PrimaryError: "load failed",
		Timeline: []outcome.TimelineEntry{
			{
				Seq:     1,
				Phase:   outcome.PhasePrimary,
				Status:  outcome.StepStatusCompleted,
				Kind:    outcome.StepKindKernelLoad,
				Target:  "prog_a",
				Details: outcome.ProgramDetails{KernelID: 123},
			},
			{
				Seq:    2,
				Phase:  outcome.PhasePrimary,
				Status: outcome.StepStatusFailed,
				Kind:   outcome.StepKindKernelLoad,
				Target: "prog_b",
				Error:  "invalid BTF",
			},
			{
				Seq:    3,
				Phase:  outcome.PhasePrimary,
				Status: outcome.StepStatusSkipped,
				Kind:   outcome.StepKindKernelLoad,
				Target: "prog_c",
			},
			{
				Seq:     4,
				Phase:   outcome.PhaseRollback,
				Status:  outcome.StepStatusCompleted,
				Kind:    outcome.StepKindKernelUnload,
				Target:  "prog_a",
				Details: outcome.ProgramDetails{KernelID: 123},
			},
		},
		Residual:              []outcome.Artefact{},
		SystemState:           "clean",
		ManualCleanupRequired: false,
		ManualCleanupCommands: nil,
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	var decoded outcome.OperationOutcome
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() failed: %v", err)
	}

	if decoded.OpID != 42 {
		t.Errorf("OpID = %d, want 42", decoded.OpID)
	}
	if decoded.Status != outcome.StatusFailure {
		t.Errorf("Status = %q, want %q", decoded.Status, outcome.StatusFailure)
	}
	if len(decoded.Timeline) != 4 {
		t.Errorf("Timeline has %d items, want 4", len(decoded.Timeline))
	}
	if decoded.PrimaryError != "load failed" {
		t.Errorf("PrimaryError = %q, want %q", decoded.PrimaryError, "load failed")
	}
	if decoded.SystemState != "clean" {
		t.Errorf("SystemState = %q, want %q", decoded.SystemState, "clean")
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

func TestTimelineSequencing(t *testing.T) {
	var out outcome.OperationOutcome
	rec := outcome.NewRecorder(&out)

	_ = rec.Complete(outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "step1"})
	_ = rec.Complete(outcome.Step{Kind: outcome.StepKindStoreSaveProgram, Target: "step2"})
	_ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, Target: "step3", Error: "failed"})
	rec.BeginRollback()
	_ = rec.RollbackComplete(outcome.Step{Kind: outcome.StepKindKernelUnload, Target: "step4"})

	if len(out.Timeline) != 4 {
		t.Fatalf("Timeline has %d entries, want 4", len(out.Timeline))
	}

	// Check sequence numbers
	for i, entry := range out.Timeline {
		expectedSeq := i + 1
		if entry.Seq != expectedSeq {
			t.Errorf("Timeline[%d].Seq = %d, want %d", i, entry.Seq, expectedSeq)
		}
	}

	// Check phases
	if out.Timeline[0].Phase != outcome.PhasePrimary {
		t.Errorf("Timeline[0].Phase = %q, want %q", out.Timeline[0].Phase, outcome.PhasePrimary)
	}
	if out.Timeline[3].Phase != outcome.PhaseRollback {
		t.Errorf("Timeline[3].Phase = %q, want %q", out.Timeline[3].Phase, outcome.PhaseRollback)
	}
}
