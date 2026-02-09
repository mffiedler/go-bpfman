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
// On failure, returns a *ManagerError containing the full operation
// outcome.
func (m *Manager) Attach(ctx context.Context, scope lock.WriterScope, spec bpfman.AttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	switch s := spec.(type) {
	case bpfman.TracepointAttachSpec:
		return m.attachTracepoint(ctx, s, opts)
	case bpfman.KprobeAttachSpec:
		return m.attachKprobe(ctx, s, opts)
	case bpfman.UprobeAttachSpec:
		return m.attachUprobe(ctx, scope, s, opts)
	case bpfman.FentryAttachSpec:
		return m.attachFentry(ctx, s, opts)
	case bpfman.FexitAttachSpec:
		return m.attachFexit(ctx, s, opts)
	case bpfman.XDPAttachSpec:
		return m.attachXDP(ctx, s, opts)
	case bpfman.TCAttachSpec:
		return m.attachTC(ctx, s, opts)
	case bpfman.TCXAttachSpec:
		return m.attachTCX(ctx, s, opts)
	default:
		return bpfman.Link{}, fmt.Errorf("unsupported attach spec type %T", spec)
	}
}
