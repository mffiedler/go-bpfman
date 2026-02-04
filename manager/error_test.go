package manager_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/outcome"
)

// TestDetach_NonExistentLink_ReturnsNotFound verifies that:
//
//	Given an empty manager with no links,
//	When I attempt to detach a non-existent link ID,
//	Then the manager returns ErrLinkNotFound with failure outcome.
func TestDetach_NonExistentLink_ReturnsNotFound(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err, "Detach of non-existent link should fail")

	var notFound bpfman.ErrLinkNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrLinkNotFound, got %T", err)
	assert.Equal(t, bpfman.LinkID(999), notFound.LinkID)

	// Verify outcome records the preflight failure
	var me *manager.ManagerError
	require.True(t, errors.As(err, &me), "expected *manager.ManagerError, got %T", err)
	o := me.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := findFailedEntry(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)
	assert.Equal(t, 0, countCompletedPrimary(o.Timeline))
	assert.Equal(t, "clean", o.SystemState)
}

// TestDetach_KernelOnlyLink_ReturnsNotManaged verifies that:
//
//	Given a link that exists in the kernel but is not managed by bpfman,
//	When I attempt to detach it,
//	Then the manager returns ErrLinkNotManaged with failure outcome.
func TestDetach_KernelOnlyLink_ReturnsNotManaged(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Inject a link directly into the kernel (bypassing bpfman)
	const kernelOnlyLinkID = 42
	fix.Kernel.InjectKernelLink(kernelOnlyLinkID, bpfman.LinkKindTracepoint)

	err := fix.Manager.Detach(ctx, bpfman.LinkID(kernelOnlyLinkID))
	require.Error(t, err, "Detach of kernel-only link should fail")

	var notManaged bpfman.ErrLinkNotManaged
	assert.True(t, errors.As(err, &notManaged), "expected ErrLinkNotManaged, got %T", err)
	assert.Equal(t, bpfman.LinkID(kernelOnlyLinkID), notManaged.LinkID)

	// Verify outcome records the preflight failure
	var me *manager.ManagerError
	require.True(t, errors.As(err, &me), "expected *manager.ManagerError, got %T", err)
	o := me.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := findFailedEntry(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)
	assert.Equal(t, 0, countCompletedPrimary(o.Timeline))
	assert.Equal(t, "clean", o.SystemState)
}

// TestUnload_NonExistentProgram_ReturnsNotFound verifies that:
//
//	Given an empty manager with no programs,
//	When I attempt to unload a non-existent program ID,
//	Then the manager returns ErrProgramNotFound with failure outcome.
func TestUnload_NonExistentProgram_ReturnsNotFound(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	err := fix.Manager.Unload(ctx, 999)
	require.Error(t, err, "Unload of non-existent program should fail")

	var notFound bpfman.ErrProgramNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrProgramNotFound, got %T", err)
	assert.Equal(t, uint32(999), notFound.ID)

	// Verify outcome records the preflight failure
	var me *manager.ManagerError
	require.True(t, errors.As(err, &me), "expected *manager.ManagerError, got %T", err)
	o := me.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := findFailedEntry(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)
	assert.Equal(t, 0, countCompletedPrimary(o.Timeline))
	assert.Equal(t, "clean", o.SystemState)
}

// TestUnload_KernelOnlyProgram_ReturnsNotManaged verifies that:
//
//	Given a program that exists in the kernel but is not managed by bpfman,
//	When I attempt to unload it,
//	Then the manager returns ErrProgramNotManaged with failure outcome.
func TestUnload_KernelOnlyProgram_ReturnsNotManaged(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Inject a program directly into the kernel (bypassing bpfman)
	const kernelOnlyProgID = 42
	fix.Kernel.InjectKernelProgram(kernelOnlyProgID, "orphan_prog", bpfman.ProgramTypeTracepoint)

	err := fix.Manager.Unload(ctx, kernelOnlyProgID)
	require.Error(t, err, "Unload of kernel-only program should fail")

	var notManaged bpfman.ErrProgramNotManaged
	assert.True(t, errors.As(err, &notManaged), "expected ErrProgramNotManaged, got %T", err)
	assert.Equal(t, uint32(kernelOnlyProgID), notManaged.ID)

	// Verify outcome records the preflight failure
	var me *manager.ManagerError
	require.True(t, errors.As(err, &me), "expected *manager.ManagerError, got %T", err)
	o := me.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := findFailedEntry(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)
	assert.Equal(t, 0, countCompletedPrimary(o.Timeline))
	assert.Equal(t, "clean", o.SystemState)
}
