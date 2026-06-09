// Package runner implements the child-side bpfman-ns command.
//
// Runner code is entered after internal/bpfman/ns has already switched the
// process into the target mount namespace from its C constructor. It should not
// call setns itself. Its job is to parse the private bpfman-ns CLI, reconstruct
// the inherited BPF program from the fd contract, verify the inherited writer
// scope, attach the requested uprobe, and return an attachment fd to the parent
// process. The writer scope is inherited from the parent manager operation
// because the attach performed here mutates kernel state and must remain
// serialised with other bpfman operations.
//
// The fd returned to the parent must be a BPF link fd that owns the live
// attachment. On modern kernels Cilium may represent uprobes as BPF perf-event
// links; in that case the owning lifetime handle is the BPF link fd, not merely
// the perf-event fd. Older ioctl-style raw perf-event attachments are rejected
// for container uprobes because the parent cannot pin them as BPF links, and an
// fd-held one-shot attach would leave a phantom store record after the command
// exits.
package runner
