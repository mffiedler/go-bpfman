package coherency

import (
	"fmt"

	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/manager/action"
)

// RepairIntent is a domain-level cleanup intent emitted by a
// coherency rule. The audit dry-run renders intents via Describe;
// the audit --repair path executes them via Actions. Concrete
// intents are sealed via the unexported marker method.
type RepairIntent interface {
	repairIntent()
	Describe() string
	Actions() []action.Action
}

// StaleXDPDispatcher removes an XDP dispatcher whose attachment is
// gone and whose extension link count is zero. XDP dispatchers carry
// an outer kernel BPF link, so the cleanup includes a link pin
// removal step that TC dispatchers do not have.
type StaleXDPDispatcher struct {
	Nsid    uint64
	Ifindex uint32
	ProgPin bpfman.ProgPinPath
	RevDir  bpfman.DispatcherRevDir
	LinkPin bpfman.LinkPath
}

func (StaleXDPDispatcher) repairIntent() {}

func (i StaleXDPDispatcher) Describe() string {
	return fmt.Sprintf("delete dispatcher %s/%d/%d and filesystem artefacts", dispatcher.DispatcherTypeXDP, i.Nsid, i.Ifindex)
}

func (i StaleXDPDispatcher) Actions() []action.Action {
	return []action.Action{
		action.RemoveDispatcherProgPin{Path: i.ProgPin},
		action.RemoveDispatcherRevDir{Path: i.RevDir},
		action.RemoveDispatcherLinkPin{Path: i.LinkPin},
		action.DeleteDispatcher{
			Type: dispatcher.DispatcherTypeXDP, Nsid: i.Nsid, Ifindex: i.Ifindex,
		},
	}
}

// StaleTCDispatcher removes a TC (ingress or egress) dispatcher
// whose attachment is gone and whose extension link count is zero.
// TC dispatchers use netlink filters, not BPF links, so there is no
// link pin to remove.
type StaleTCDispatcher struct {
	Type    dispatcher.DispatcherType // TCIngress or TCEgress
	Nsid    uint64
	Ifindex uint32
	ProgPin bpfman.ProgPinPath
	RevDir  bpfman.DispatcherRevDir
}

func (StaleTCDispatcher) repairIntent() {}

func (i StaleTCDispatcher) Describe() string {
	return fmt.Sprintf("delete dispatcher %s/%d/%d and filesystem artefacts", i.Type, i.Nsid, i.Ifindex)
}

func (i StaleTCDispatcher) Actions() []action.Action {
	return []action.Action{
		action.RemoveDispatcherProgPin{Path: i.ProgPin},
		action.RemoveDispatcherRevDir{Path: i.RevDir},
		action.DeleteDispatcher{
			Type: i.Type, Nsid: i.Nsid, Ifindex: i.Ifindex,
		},
	}
}

// RemoveOrphanArtefact removes a single orphaned filesystem artefact.
// Kind discriminates which typed action realises the removal. Path is
// the raw filesystem path captured at gather time; Actions wraps it in
// the appropriate path newtype.
type RemoveOrphanArtefact struct {
	Kind OrphanKind
	Path string
}

func (RemoveOrphanArtefact) repairIntent() {}

func (i RemoveOrphanArtefact) Describe() string {
	switch i.Kind {
	case OrphanProgramDir, OrphanProgramDirUnk:
		return fmt.Sprintf("remove program dir %s", i.Path)
	case OrphanSharedMapPin:
		return fmt.Sprintf("remove shared map pin %s", i.Path)
	case OrphanStagingDir:
		return fmt.Sprintf("remove staging dir %s", i.Path)
	default:
		return fmt.Sprintf("remove %s", i.Path)
	}
}

func (i RemoveOrphanArtefact) Actions() []action.Action {
	switch i.Kind {
	case OrphanProgPin:
		return []action.Action{action.RemoveProgPin{Path: bpfman.ProgPinPath(i.Path)}}
	case OrphanLinkDir:
		return []action.Action{action.RemoveLinkDir{Path: bpfman.LinkDir(i.Path)}}
	case OrphanMapDir:
		return []action.Action{action.RemoveMapDir{Path: bpfman.MapDir(i.Path)}}
	case OrphanDispatcherDir:
		return []action.Action{action.RemoveDispatcherRevDir{Path: bpfman.DispatcherRevDir(i.Path)}}
	case OrphanDispatcherLink:
		return []action.Action{action.RemoveDispatcherLinkPin{Path: bpfman.LinkPath(i.Path)}}
	case OrphanProgramDir, OrphanProgramDirUnk:
		return []action.Action{action.RemoveProgramDir{Path: i.Path}}
	case OrphanSharedMapPin:
		return []action.Action{action.RemoveSharedMapPin{Path: bpfman.MapPinPath(i.Path)}}
	case OrphanStagingDir:
		return []action.Action{action.RemoveStagingDir{Path: i.Path}}
	default:
		panic(fmt.Sprintf("RemoveOrphanArtefact: unexpected kind %s", i.Kind))
	}
}
