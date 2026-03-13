package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/lock"
)

// Attach attaches a loaded program using the given spec.  The spec
// type determines which internal attach path is used.  The scope
// parameter is required for container uprobes (where the lock fd must
// be passed to a helper subprocess); for all other types it may be
// nil.
//
// On failure, returns a plain error. Completed steps are rolled back
// automatically by the plan interpreter.
func (m *Manager) Attach(ctx context.Context, scope lock.WriterScope, spec bpfman.AttachSpec) (_ bpfman.Link, retErr error) {
	ctx, err := m.beginOp(ctx)
	if err != nil {
		return bpfman.Link{}, err
	}
	defer func() { m.endOp(retErr) }()

	switch s := spec.(type) {
	case bpfman.TracepointAttachSpec:
		return m.attachTracepoint(ctx, s)
	case bpfman.KprobeAttachSpec:
		return m.attachKprobe(ctx, s)
	case bpfman.UprobeAttachSpec:
		return m.attachUprobe(ctx, scope, s)
	case bpfman.FentryAttachSpec:
		return m.attachFentry(ctx, s)
	case bpfman.FexitAttachSpec:
		return m.attachFexit(ctx, s)
	case bpfman.XDPAttachSpec:
		return m.attachXDP(ctx, s)
	case bpfman.TCAttachSpec:
		return m.attachTC(ctx, s)
	case bpfman.TCXAttachSpec:
		return m.attachTCX(ctx, s)
	default:
		return bpfman.Link{}, fmt.Errorf("unsupported attach spec type %T", spec)
	}
}
