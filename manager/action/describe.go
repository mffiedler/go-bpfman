package action

import "fmt"

// Describe returns a human-readable description of an action.
func Describe(a Action) string {
	switch a := a.(type) {
	// Store actions
	case SaveProgram:
		return fmt.Sprintf("save program %d to store", a.KernelID)
	case DeleteProgram:
		return fmt.Sprintf("delete program %d from store", a.KernelID)
	case SaveLink:
		return fmt.Sprintf("save link %d to store", a.Record.ID)
	case DeleteLink:
		return fmt.Sprintf("delete link %d from store", a.LinkID)
	case GetProgramFromStore:
		return fmt.Sprintf("get program %d from store", a.KernelID)
	case CheckProgramNotInStore:
		return fmt.Sprintf("verify program %d not in store", a.KernelID)

	// Kernel load/unload
	case LoadProgram:
		return fmt.Sprintf("load program %s", a.Spec.ProgramName())
	case UnloadProgram:
		return fmt.Sprintf("unload program at %s", a.PinPath)

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

	// Link/pin actions
	case DetachLink:
		return fmt.Sprintf("detach link at %s", a.PinPath)
	case RemovePin:
		return fmt.Sprintf("remove pin %s", a.Path)
	case PublishBytecode:
		return fmt.Sprintf("publish bytecode for program %d", a.KernelID)

	// Dispatcher actions
	case SaveDispatcher:
		return fmt.Sprintf("save %s dispatcher nsid=%d ifindex=%d", a.State.Type, a.State.Nsid, a.State.Ifindex)
	case DeleteDispatcher:
		return fmt.Sprintf("delete %s dispatcher nsid=%d ifindex=%d from store", a.Type, a.Nsid, a.Ifindex)
	case EnsureXDPDispatcher:
		return fmt.Sprintf("ensure XDP dispatcher ifindex=%d", a.Ifindex)
	case EnsureTCDispatcher:
		return fmt.Sprintf("ensure TC dispatcher ifindex=%d %s", a.Ifindex, a.Direction)
	case AttachXDPExtension:
		return fmt.Sprintf("attach XDP extension %s", a.ProgramName)
	case AttachTCExtension:
		return fmt.Sprintf("attach TC extension %s", a.ProgramName)

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
	case RemoveProgramDirByPath:
		return fmt.Sprintf("remove program directory %s", a.Path)
	case RemoveStagingDir:
		return fmt.Sprintf("remove staging directory %s", a.Path)
	case RemoveProgramDir:
		return fmt.Sprintf("remove program directory for %d", a.KernelID)
	case DetachTCFilter:
		return fmt.Sprintf("detach TC filter ifindex=%d priority=%d", a.Ifindex, a.Priority)

	default:
		return fmt.Sprintf("%T", a)
	}
}
