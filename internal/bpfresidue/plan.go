package bpfresidue

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	bpfnetns "github.com/frobware/go-bpfman/ns/netns"
)

// Action is one cleanup step. Describe returns the shell-shaped
// line shown in dry-run output (so a reader can audit it and, in
// principle, run it by hand); Apply executes the same step
// against the live system. Action values are pure data --
// describing them allocates no resources -- so Plan can be built,
// dedup'd, and reordered without touching the kernel.
type Action interface {
	Describe() string
	Apply() error
}

// Plan is an ordered list of Actions. Order is load-bearing: pin
// removal must precede netdev deletion (otherwise the kernel
// leaves a detached link object pinned), and netdev deletion
// must precede netns deletion (otherwise we cascade twice).
// Scanners produce a Plan already in the right order; callers
// assembling a composite plan append in order and rely on each
// scanner's own ordering.
type Plan []Action

// Describe writes one line per action to w. Pure.
func (p Plan) Describe(w io.Writer) {
	for _, a := range p {
		fmt.Fprintln(w, a.Describe())
	}
}

// ActionFailure pairs an Action with the error its Apply
// returned. Apply collects one of these per failing action and
// hands them all back to the caller, which is the right place
// to decide how to render them -- callers running interactively
// want one line per failure with the action's Describe() as the
// preamble.
type ActionFailure struct {
	Action Action
	Err    error
}

// Apply executes every action. Per-entry errors are accumulated
// so a single bad step does not block the rest -- the cleanup
// tool's purpose is to drain as much as possible on each run.
// Returns the per-action failures in execution order; an empty
// slice means every action succeeded.
func (p Plan) Apply() []ActionFailure {
	var failures []ActionFailure
	for _, a := range p {
		if err := a.Apply(); err != nil {
			failures = append(failures, ActionFailure{Action: a, Err: err})
		}
	}
	return failures
}

// Empty reports whether the plan carries no actions.
func (p Plan) Empty() bool { return len(p) == 0 }

// Dedup removes duplicate RemovePin actions (same Path), keeping
// the first occurrence and preserving order. Other action kinds
// are passed through unchanged; their identity is implicit in
// the kernel object they target and the scanners that produce
// them do not generate duplicates in practice.
func (p Plan) Dedup() Plan {
	if len(p) <= 1 {
		return p
	}
	seenPin := map[string]bool{}
	out := make(Plan, 0, len(p))
	for _, a := range p {
		if rp, ok := a.(RemovePin); ok {
			if seenPin[rp.Path] {
				continue
			}
			seenPin[rp.Path] = true
		}
		out = append(out, a)
	}
	return out
}

// RemoveTree recursively removes a directory tree. Used by the
// --nuke path to clear bpfman's runtime FS subtree wholesale,
// ignoring DB ownership. Missing trees are not an error.
type RemoveTree struct {
	Path string
}

// Describe implements Action.
func (a RemoveTree) Describe() string { return fmt.Sprintf("rm -rf -- %s", a.Path) }

// Apply implements Action.
func (a RemoveTree) Apply() error {
	if err := os.RemoveAll(a.Path); err != nil {
		return fmt.Errorf("remove tree %s: %w", a.Path, err)
	}
	return nil
}

// RemovePin unlinks a pin file under the bpf fs. Removing the
// last reference to a bpf_link makes the kernel detach the link
// and GC the program; for program / map pins the kernel GCs the
// object once no FD or other pin keeps it alive.
type RemovePin struct {
	Path string
}

// Describe implements Action.
func (a RemovePin) Describe() string { return fmt.Sprintf("rm -f -- %s", a.Path) }

// Apply implements Action.
func (a RemovePin) Apply() error {
	if err := os.Remove(a.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", a.Path, err)
	}
	return nil
}

// DetachLink closes a bpf_link by ID. Used for kernel-orphan
// links not backed by any pin -- closing the only FD makes the
// kernel detach. If the link is already gone (someone else
// closed it between scan and apply), Apply succeeds quietly.
type DetachLink struct {
	ID link.ID
}

// Describe implements Action. The shell-shaped output uses
// bpftool because that is the closest hand-runnable equivalent;
// Apply does the same work via the bpf_link API directly.
func (a DetachLink) Describe() string { return fmt.Sprintf("bpftool link detach id %d", a.ID) }

// Apply implements Action.
func (a DetachLink) Apply() error {
	lnk, err := link.NewFromID(a.ID)
	if err != nil {
		// Already gone, not an error worth surfacing.
		return nil
	}
	if err := lnk.Close(); err != nil {
		return fmt.Errorf("detach link %d: %w", a.ID, err)
	}
	return nil
}

// DeleteQdisc removes a clsact qdisc from an interface,
// cascading the ingress and egress filters it carries. NetnsPath
// empty selects the current netns; non-empty enters via setns.
// NetnsName is only used by Describe.
type DeleteQdisc struct {
	NetnsPath string
	NetnsName string
	IfName    string
}

// Describe implements Action.
func (a DeleteQdisc) Describe() string {
	if a.NetnsName == "" {
		return fmt.Sprintf("tc qdisc del dev %s clsact", a.IfName)
	}
	return fmt.Sprintf("ip netns exec %s tc qdisc del dev %s clsact", a.NetnsName, a.IfName)
}

// Apply implements Action.
func (a DeleteQdisc) Apply() error {
	work := func() error {
		l, err := netlink.LinkByName(a.IfName)
		if err != nil {
			return fmt.Errorf("look up %s: %w", a.IfName, err)
		}
		qdiscs, err := netlink.QdiscList(l)
		if err != nil {
			return fmt.Errorf("list qdiscs on %s: %w", a.IfName, err)
		}
		for _, q := range qdiscs {
			if q.Type() != "clsact" {
				continue
			}
			if err := netlink.QdiscDel(q); err != nil {
				return fmt.Errorf("delete clsact on %s: %w", a.IfName, err)
			}
			return nil
		}
		return nil // already gone
	}
	if a.NetnsPath == "" {
		return work()
	}
	if err := bpfnetns.Run(a.NetnsPath, work); err != nil {
		return fmt.Errorf("netns %s: %w", a.NetnsName, err)
	}
	return nil
}

// DeleteIface removes a network interface from the current
// netns. Veth peers cascade automatically: deleting the host
// end of an `Na` / `Nb` pair tears down the peer wherever it
// lives.
type DeleteIface struct {
	Name string
}

// Describe implements Action.
func (a DeleteIface) Describe() string { return fmt.Sprintf("ip link del %s", a.Name) }

// Apply implements Action.
func (a DeleteIface) Apply() error {
	l, err := netlink.LinkByName(a.Name)
	if err != nil {
		return fmt.Errorf("look up %s: %w", a.Name, err)
	}
	if err := netlink.LinkDel(l); err != nil {
		return fmt.Errorf("delete %s: %w", a.Name, err)
	}
	return nil
}

// DeleteNetns removes a named netns under /run/netns. The
// vishvananda/netns library hardcodes that directory; the
// netnsDir parameter on the scanner exists for test isolation
// only.
type DeleteNetns struct {
	Name string
}

// Describe implements Action.
func (a DeleteNetns) Describe() string { return fmt.Sprintf("ip netns del %s", a.Name) }

// Apply implements Action.
func (a DeleteNetns) Apply() error {
	if err := netns.DeleteNamed(a.Name); err != nil {
		return fmt.Errorf("delete netns %s: %w", a.Name, err)
	}
	return nil
}
