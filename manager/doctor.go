package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/manager/coherency"
)

// Doctor gathers state and evaluates all coherency rules.
func (m *Manager) Doctor(ctx context.Context) (coherency.DoctorReport, error) {
	state, err := coherency.GatherState(ctx, m.store, m.kernel, m.Layout())
	if err != nil {
		return coherency.DoctorReport{}, fmt.Errorf("gather state: %w", err)
	}

	violations := coherency.Evaluate(state, coherency.CoherencyRules())

	var report coherency.DoctorReport
	for _, v := range violations {
		report.Findings = append(report.Findings, v.Finding())
	}
	return report, nil
}

// CoherencyGC gathers state, evaluates GC rules, and executes
// planned operations. Returns the number of operations applied.
// This handles stale dispatchers and orphan filesystem artefacts.
// Store-level GC (structural cleanup) is handled separately by
// store.GC() called from Manager.GC().
func (m *Manager) CoherencyGC(ctx context.Context) (int, error) {
	state, err := coherency.GatherState(ctx, m.store, m.kernel, m.Layout())
	if err != nil {
		return 0, fmt.Errorf("gather state: %w", err)
	}

	violations := coherency.Evaluate(state, coherency.GCRules())

	applied := 0
	for _, v := range violations {
		if v.Op == nil {
			continue
		}
		if err := v.Op.Execute(); err != nil {
			m.logger.WarnContext(ctx, "gc operation failed",
				"op", v.Op.Description,
				"error", err)
			continue
		}
		m.logger.InfoContext(ctx, "gc operation applied", "op", v.Op.Description)
		applied++
	}
	return applied, nil
}
