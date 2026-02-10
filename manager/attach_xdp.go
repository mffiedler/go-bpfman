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
// The dispatcher is created automatically if it doesn't exist for the interface.
// Programs are attached as extensions (freplace) to dispatcher slots.
// The program is reloaded from its original ObjectPath as Extension type.
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
		programKernelID: spec.ProgramID(),
		ifindex:         ifindex,
		ifname:          ifname,
		netnsPath:       netnsPath,
		target:          ifname + ":xdp",
		dispType:        dispatcher.DispatcherTypeXDP,
		ensureAction: func() action.Action {
			return action.EnsureXDPDispatcher{
				Ifindex:   uint32(ifindex),
				NetnsPath: netnsPath,
			}
		},
		extensionAction: func(ds dispatcher.State, prog bpfman.ProgramRecord) action.Action {
			return action.AttachXDPExtension{
				DispState:   ds,
				NetnsPath:   netnsPath,
				ObjectPath:  prog.Load.ObjectPath(),
				ProgramName: prog.Meta.Name,
				MapPinDir:   prog.Handles.MapPinPath,
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
				DispatcherID: dispState.KernelID,
				Revision:     dispState.Revision,
			}
		},
	})
}
