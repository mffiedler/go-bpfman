package action

import "fmt"

// Describe returns a human-readable description of an action. Actions
// relevant to GC have polished descriptions; all others fall through
// to the type name.
func Describe(a Action) string {
	switch a := a.(type) {
	case DeleteProgram:
		return fmt.Sprintf("delete program %d from store", a.KernelID)
	case DeleteLink:
		return fmt.Sprintf("delete link %d from store", a.LinkID)
	case DeleteDispatcher:
		return fmt.Sprintf("delete %s dispatcher nsid=%d ifindex=%d from store", a.Type, a.Nsid, a.Ifindex)
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
