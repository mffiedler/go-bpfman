// CLI helpers duplicated from cmd/bpfman: small operational helpers
// that the shell Command ADT shares with bpfman's Kong subcommands.
// The duplication keeps the two binaries independent at the package
// boundary; if these grow further or drift, lift them into a shared
// internal package.
package bpfmanbuiltin

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
)

// loadFileResult captures the result of a load file operation.
type loadFileResult struct {
	Programs []bpfman.Program
}

func collectDeleteIDs(ctx context.Context, mgr *manager.Manager, all bool, explicit []kernel.ProgramID) ([]kernel.ProgramID, error) {
	return mgr.ResolveDeleteProgramIDs(ctx, all, explicit)
}

// executeDeletePrograms is the shared implementation for deleting
// programs with cascading cleanup. Locking is handled internally.
func executeDeletePrograms(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, ids []kernel.ProgramID, recursive bool) error {
	type result struct {
		id  kernel.ProgramID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := bpfmancli.RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		deleteResults := mgr.DeletePrograms(ctx, writeLock, ids, manager.DeleteProgramsOpts{Recursive: recursive})
		for _, r := range deleteResults {
			results = append(results, result{id: r.ProgramID, err: r.Err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("program %d: %v\n", r.id, r.err)
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d program(s) failed to delete", failCount, len(results))
	}

	return nil
}
