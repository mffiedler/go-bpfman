package manager

import (
	"context"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/manager/action"
)

// xdpProceedOnPass is the proceed-on bitmask for XDP_PASS.
var xdpProceedOnPass = dispatcher.ProceedOnMask(dispatcher.XDPPass)

// attachXDP attaches an XDP program to a network interface using the
// dispatcher model for multi-program chaining.
//
// Every attach triggers a full dispatcher rebuild: a new dispatcher
// is loaded with updated .rodata config, all extensions are re-attached,
// and the XDP link is atomically swapped (or created for first attach).
//
// Pin paths follow the Rust bpfman convention:
//   - Dispatcher link: /sys/fs/bpf/bpfman/xdp/dispatcher_{nsid}_{ifindex}_link
//   - Dispatcher prog: /sys/fs/bpf/bpfman/xdp/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher
//   - Extension links: /sys/fs/bpf/bpfman/xdp/dispatcher_{nsid}_{ifindex}_{revision}/link_{position}
func (m *Manager) attachXDP(ctx context.Context, spec bpfman.XDPAttachSpec) (bpfman.Link, error) {
	ifname := spec.Ifname()
	ifindex := spec.Ifindex()
	netnsPath := spec.Netns()

	return m.dispatcherAttach(ctx, dispatcherAttachParams{
		programID: spec.ProgramID(),
		ifindex:   ifindex,
		ifname:    ifname,
		netnsPath: netnsPath,
		target:    ifname + ":xdp",
		dispType:  dispatcher.DispatcherTypeXDP,
		rebuildAction: func(prog bpfman.ProgramRecord) action.Action {
			return action.RebuildXDPDispatcher{
				Ifindex:     uint32(ifindex),
				Ifname:      ifname,
				NetnsPath:   netnsPath,
				ProgPinPath: prog.Handles.PinPath,
				ProgramName: prog.Meta.Name,
				Priority:    dispatcher.DefaultPriority,
				ProceedOn:   xdpProceedOnPass,
			}
		},
		buildLinkDetails: func(nsid uint64, position int, dispState dispatcher.State) bpfman.LinkDetails {
			return bpfman.XDPDetails{
				Interface:    ifname,
				Ifindex:      uint32(ifindex),
				Priority:     dispatcher.DefaultPriority,
				Position:     int32(position),
				ProceedOn:    []int32{int32(dispatcher.XDPPass)},
				Nsid:         nsid,
				DispatcherID: dispState.ProgramID,
				Revision:     dispState.Revision,
			}
		},
	})
}
