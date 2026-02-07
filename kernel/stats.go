package kernel

import "time"

// ProgramStats contains runtime statistics for a BPF program.
// Stats collection must be enabled via sysctl kernel.bpf_stats_enabled=1.
//
// Requirements:
//   - Linux 5.8+ for Runtime/RunCount
//   - Linux 5.12+ for RecursionMisses
type ProgramStats struct {
	Runtime         time.Duration `json:"runtime"`
	RunCount        uint64        `json:"run_count"`
	RecursionMisses uint64        `json:"recursion_misses"`
}
