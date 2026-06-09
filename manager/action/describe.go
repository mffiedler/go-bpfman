package action

import "fmt"

// Describe returns a human-readable description of an action.
func Describe(a Action) string {
	switch a := a.(type) {
	// Store actions
	case SaveProgram:
		return fmt.Sprintf("save program %d to store", a.ProgramID)
	case DeleteProgram:
		return fmt.Sprintf("delete program %d from store", a.ProgramID)
	case CreateLink:
		return "create link in store"
	case DeleteLink:
		return fmt.Sprintf("delete link %d from store", a.LinkID)
	case GetProgramFromStore:
		return fmt.Sprintf("get program %d from store", a.ProgramID)
	case CheckProgramNotInStore:
		return fmt.Sprintf("verify program %d not in store", a.ProgramID)

	// Kernel load/unload
	case LoadProgram:
		return fmt.Sprintf("load program %s", a.Spec.ProgramName())
	case UnloadProgram:
		return fmt.Sprintf("unload program at %s", a.PinPath)
	case RemoveMapsPins:
		return fmt.Sprintf("remove maps pins at %s", a.PinPath)

	// Attach actions
	case AttachTracepoint:
		return fmt.Sprintf("attach tracepoint %s/%s", a.Group, a.Name)
	case AttachKprobe:
		return fmt.Sprintf("attach kprobe %s", a.FnName)
	case AttachUprobeLocal:
		return fmt.Sprintf("attach uprobe %s:%s", a.Target, a.FnName)
	case AttachUprobeContainer:
		return fmt.Sprintf("attach uprobe (container) %s:%s", a.Target, a.FnName)
	case AttachFentry:
		return fmt.Sprintf("attach fentry %s", a.FnName)
	case AttachFexit:
		return fmt.Sprintf("attach fexit %s", a.FnName)
	case AttachTCX:
		return fmt.Sprintf("attach TCX ifindex=%d %s", a.Ifindex, a.Direction)

	// Link/pin actions
	case DetachLink:
		return fmt.Sprintf("detach link at %s", a.PinPath)
	case PublishBytecode:
		return fmt.Sprintf("publish bytecode for program %d", a.ProgramID)

	// Dispatcher actions
	case DeleteDispatcher:
		return fmt.Sprintf("delete %s dispatcher nsid=%d ifindex=%d from store", a.Type, a.Nsid, a.Ifindex)
	case RebuildXDPDispatcher:
		return fmt.Sprintf("rebuild XDP dispatcher ifindex=%d", a.Ifindex)
	case RebuildTCDispatcher:
		return fmt.Sprintf("rebuild TC dispatcher ifindex=%d %s", a.Ifindex, a.Direction)
	case RebuildDispatcherForDetach:
		return fmt.Sprintf("rebuild %s dispatcher for detach nsid=%d ifindex=%d excluding link %d", a.Key.Type, a.Key.Nsid, a.Key.Ifindex, a.ExcludeLinkID)
	case RemoveDispatcher:
		return fmt.Sprintf("remove %s dispatcher nsid=%d ifindex=%d", a.Key.Type, a.Key.Nsid, a.Key.Ifindex)

	// GC filesystem cleanup
	case RemoveProgPin:
		return fmt.Sprintf("remove program pin %s", a.Path)
	case RemoveLinkDir:
		return fmt.Sprintf("remove link directory %s", a.Path)
	case RemoveMapDir:
		return fmt.Sprintf("remove map directory %s", a.Path)
	case RemoveDispatcherProgPin:
		return fmt.Sprintf("remove dispatcher program pin %s", a.Path)
	case RemoveDispatcherRevDir:
		return fmt.Sprintf("remove dispatcher revision directory %s", a.Path)
	case RemoveDispatcherLinkPin:
		return fmt.Sprintf("remove dispatcher link pin %s", a.Path)
	case RemoveProgramDir:
		return fmt.Sprintf("remove program directory %s", a.Path)
	case RemoveStagingDir:
		return fmt.Sprintf("remove staging directory %s", a.Path)
	case DetachTCFilter:
		return fmt.Sprintf("detach TC filter ifindex=%d priority=%d", a.Ifindex, a.Priority)
	default:
		return fmt.Sprintf("%T", a)
	}
}
