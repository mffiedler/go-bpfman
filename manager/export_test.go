package manager

import "context"

// NewExecutorForTest exposes the unexported newExecutor for black-box tests.
var NewExecutorForTest = newExecutor

// ReapDeadProgramRecordsForTest exposes the unexported reap for black-box
// tests.
func (m *Manager) ReapDeadProgramRecordsForTest(ctx context.Context) error {
	return m.reapDeadProgramRecords(ctx)
}
