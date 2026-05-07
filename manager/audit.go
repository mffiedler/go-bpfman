package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/manager/coherency"
)

// Audit gathers state and evaluates all coherency rules. The
// returned report carries raw violations with their RepairIntents
// preserved so callers can render planned actions; use Findings()
// for the projected diagnostic view.
func (m *Manager) Audit(ctx context.Context) (coherency.AuditReport, error) {
	state, err := coherency.GatherState(ctx, m.store, m.kernel, m.Layout())
	if err != nil {
		return coherency.AuditReport{}, fmt.Errorf("gather state: %w", err)
	}

	return coherency.AuditReport{
		Violations: coherency.Evaluate(state, coherency.Rules()),
	}, nil
}
