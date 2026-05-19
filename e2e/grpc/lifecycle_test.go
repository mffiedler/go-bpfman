//go:build e2e

package grpcparallel

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/frobware/go-bpfman/e2e/testnet"
	"github.com/frobware/go-bpfman/e2e/uprobetarget"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// typeSpec captures the per-program-type knobs the shared
// lifecycle helper needs: where the bytecode lives, the program
// name inside it, the enum value, optional load-time
// ProgSpecificInfo (FentryLoadInfo / FexitLoadInfo / ...), and a
// per-goroutine attach builder.
//
// attachBuilder runs once per goroutine to produce a per-iteration
// AttachInfo closure. Any state the goroutine needs across its
// iterations (e.g. the host-side netif name for the XDP/TC/TCX
// types, which create their own veth at goroutine setup time) is
// captured in the returned closure. That keeps per-goroutine
// state out of typeSpec's signature and away from
// interface{}-typed plumbing.
type typeSpec struct {
	name          string
	object        string // basename under testdataDir
	progName      string
	enumType      pb.BpfmanProgramType
	loadInfo      *pb.ProgSpecificInfo
	attachBuilder func(t *testing.T, gid int) func() *pb.AttachInfo
}

// TestParallel_GRPC runs each program type's lifecycle as a
// separate sub-test. Sub-tests call t.Parallel(), so Go's test
// framework drives them concurrently against the single daemon;
// each sub-test also fans goroutines internally for within-type
// parallelism. The daemon therefore observes
// load/attach/detach/unload of multiple program types
// interleaved, which is the surface that matters for the
// in-process serialisation removal.
func TestParallel_GRPC(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (bpfman load)")
	}

	specs := []typeSpec{
		kprobeSpec(),
		tracepointSpec(),
		fentrySpec(),
		fexitSpec(),
		uprobeSpec(),
		xdpSpec(),
		tcSpec(),
		tcxSpec(),
	}

	for _, spec := range specs {
		spec := spec
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()
			runParallelLifecycles(t, spec)
		})
	}
}

// runParallelLifecycles fans BPFMAN_GRPC_PARALLEL_N goroutines,
// each running BPFMAN_GRPC_PARALLEL_ITERS independent lifecycles
// of the given type. Failures inside a goroutine stop that
// goroutine and surface as t.Error after wg.Wait, so siblings
// keep running and we see the full failure set rather than just
// the first one.
func runParallelLifecycles(t *testing.T, spec typeSpec) {
	// Defaults are sized so the full multi-type matrix
	// (per-sub-test N x ITERS x number-of-sub-tests) completes
	// in roughly 15-20s under typical contention. Crank either
	// knob via env for stress runs; lifecycles serialise on the
	// daemon's writer flock for mutating RPCs, so total wall
	// time scales linearly with N x ITERS x sub-tests.
	n := envInt("BPFMAN_GRPC_PARALLEL_N", 16)
	iters := envInt("BPFMAN_GRPC_PARALLEL_ITERS", 2)

	var wg sync.WaitGroup
	errCh := make(chan error, n*iters)

	for g := 0; g < n; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			buildAttach := spec.attachBuilder(t, gid)
			for i := 0; i < iters; i++ {
				if err := runOneLifecycle(t, spec, buildAttach, gid, i); err != nil {
					errCh <- fmt.Errorf("%s g%d iter%d: %w", spec.name, gid, i, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// runOneLifecycle drives one Load -> Get -> Attach -> GetLink ->
// ListLinks -> Detach -> Unload cycle for the given type, with
// round-trip and post-condition assertions at each step. No
// counter-delta or shape assertions: those live in the .bpfman
// scripts under e2e/new/. We only verify that the daemon's gRPC
// surface behaves correctly under concurrency.
func runOneLifecycle(t *testing.T, spec typeSpec, buildAttach func() *pb.AttachInfo, gid, iter int) error {
	// 60s per-iteration safety net. With 5 sub-tests fanning in
	// parallel, a goroutine can wait up to (N x sub-tests) flock
	// acquisitions x ~50ms ≈ tens of seconds in the worst case
	// before its Attach completes. 60s is generous headroom for
	// the default knobs and still bounded if something wedges.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	loadInfo := &pb.LoadInfo{
		Name:        spec.progName,
		ProgramType: spec.enumType,
		Info:        spec.loadInfo,
	}
	loadResp, err := client.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: testdataPath(spec.object)},
		},
		Info: []*pb.LoadInfo{loadInfo},
		Metadata: map[string]string{
			"test":      "grpc_parallel",
			"spec":      spec.name,
			"goroutine": strconv.Itoa(gid),
			"iter":      strconv.Itoa(iter),
		},
	})
	if err != nil {
		return fmt.Errorf("Load: %w", err)
	}
	if len(loadResp.Programs) != 1 {
		return fmt.Errorf("Load: want 1 program, got %d", len(loadResp.Programs))
	}
	if loadResp.Programs[0].KernelInfo == nil {
		return fmt.Errorf("Load: missing KernelInfo")
	}
	progID := loadResp.Programs[0].KernelInfo.Id

	getResp, err := client.Get(ctx, &pb.GetRequest{Id: progID})
	if err != nil {
		return fmt.Errorf("Get %d: %w", progID, err)
	}
	if getResp.KernelInfo == nil || getResp.KernelInfo.Id != progID {
		return fmt.Errorf("Get %d: id mismatch", progID)
	}
	if got := getResp.Info.Metadata["goroutine"]; got != strconv.Itoa(gid) {
		return fmt.Errorf("Get %d: metadata.goroutine %q != %d", progID, got, gid)
	}

	attachResp, err := client.Attach(ctx, &pb.AttachRequest{
		Id:     progID,
		Attach: buildAttach(),
	})
	if err != nil {
		return fmt.Errorf("Attach %d: %w", progID, err)
	}
	linkID := attachResp.LinkId

	getLinkResp, err := client.GetLink(ctx, &pb.GetLinkRequest{KernelLinkId: linkID})
	if err != nil {
		return fmt.Errorf("GetLink %d: %w", linkID, err)
	}
	if getLinkResp.Link == nil || getLinkResp.Link.Summary == nil ||
		getLinkResp.Link.Summary.KernelLinkId != linkID {
		return fmt.Errorf("GetLink %d: link id mismatch", linkID)
	}

	listResp, err := client.ListLinks(ctx, &pb.ListLinksRequest{ProgramId: &progID})
	if err != nil {
		return fmt.Errorf("ListLinks for program %d: %w", progID, err)
	}
	found := false
	for _, l := range listResp.Links {
		if l.Summary != nil && l.Summary.KernelLinkId == linkID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("ListLinks: link %d missing for program %d", linkID, progID)
	}

	if _, err := client.Detach(ctx, &pb.DetachRequest{LinkId: linkID}); err != nil {
		return fmt.Errorf("Detach %d: %w", linkID, err)
	}
	if _, err := client.GetLink(ctx, &pb.GetLinkRequest{KernelLinkId: linkID}); err == nil {
		return fmt.Errorf("post-Detach: GetLink %d still succeeds", linkID)
	}

	if _, err := client.Unload(ctx, &pb.UnloadRequest{Id: progID}); err != nil {
		return fmt.Errorf("Unload %d: %w", progID, err)
	}
	if _, err := client.Get(ctx, &pb.GetRequest{Id: progID}); err == nil {
		return fmt.Errorf("post-Unload: Get %d still succeeds", progID)
	}

	return nil
}

// ---------------------------------------------------------------
// Per-type specs.
// ---------------------------------------------------------------

// constantAttachBuilder lifts a per-iteration AttachInfo into
// the typeSpec.attachBuilder signature. Use for types whose
// AttachInfo never varies across goroutines or iterations
// (kprobe / tracepoint / fentry / fexit) -- the per-goroutine
// setup is a no-op and the same value is returned for every
// iteration.
func constantAttachBuilder(info *pb.AttachInfo) func(*testing.T, int) func() *pb.AttachInfo {
	return func(_ *testing.T, _ int) func() *pb.AttachInfo {
		return func() *pb.AttachInfo { return info }
	}
}

func kprobeSpec() typeSpec {
	return typeSpec{
		name:     "kprobe",
		object:   "kprobe_counter.bpf.o",
		progName: "kprobe_counter",
		enumType: pb.BpfmanProgramType_KPROBE,
		attachBuilder: constantAttachBuilder(&pb.AttachInfo{
			Info: &pb.AttachInfo_KprobeAttachInfo{
				KprobeAttachInfo: &pb.KprobeAttachInfo{FnName: "do_unlinkat"},
			},
		}),
	}
}

func tracepointSpec() typeSpec {
	return typeSpec{
		name:     "tracepoint",
		object:   "tracepoint_counter.bpf.o",
		progName: "tracepoint_kill_recorder",
		enumType: pb.BpfmanProgramType_TRACEPOINT,
		attachBuilder: constantAttachBuilder(&pb.AttachInfo{
			Info: &pb.AttachInfo_TracepointAttachInfo{
				TracepointAttachInfo: &pb.TracepointAttachInfo{
					Tracepoint: "syscalls/sys_enter_kill",
				},
			},
		}),
	}
}

func fentrySpec() typeSpec {
	return typeSpec{
		name:     "fentry",
		object:   "fentry_counter.bpf.o",
		progName: "test_fentry",
		enumType: pb.BpfmanProgramType_FENTRY,
		loadInfo: &pb.ProgSpecificInfo{
			Info: &pb.ProgSpecificInfo_FentryLoadInfo{
				FentryLoadInfo: &pb.FentryLoadInfo{FnName: "do_unlinkat"},
			},
		},
		attachBuilder: constantAttachBuilder(&pb.AttachInfo{
			Info: &pb.AttachInfo_FentryAttachInfo{
				FentryAttachInfo: &pb.FentryAttachInfo{},
			},
		}),
	}
}

func fexitSpec() typeSpec {
	return typeSpec{
		name:     "fexit",
		object:   "fentry_counter.bpf.o",
		progName: "test_fexit",
		enumType: pb.BpfmanProgramType_FEXIT,
		loadInfo: &pb.ProgSpecificInfo{
			Info: &pb.ProgSpecificInfo_FexitLoadInfo{
				FexitLoadInfo: &pb.FexitLoadInfo{FnName: "do_unlinkat"},
			},
		},
		attachBuilder: constantAttachBuilder(&pb.AttachInfo{
			Info: &pb.AttachInfo_FexitAttachInfo{
				FexitAttachInfo: &pb.FexitAttachInfo{},
			},
		}),
	}
}

// keepUprobeTargetLive pins uprobetarget.Invoke into the
// binary's reachable-symbol set so the Go linker does not
// dead-code-eliminate the cgo wrapper and, with it, the C
// symbol the uprobe sub-test attaches to. The test never calls
// Invoke at runtime -- it only attaches a uprobe to the symbol's
// ELF address.
var keepUprobeTargetLive = uprobetarget.Invoke

func uprobeSpec() typeSpec {
	fnName := uprobetarget.Symbol
	// The uprobe attaches to a cgo'd C symbol in *this* test
	// binary, not in the daemon. /proc/self/exe is daemon-relative
	// once the request reaches bpfman, so resolve the test
	// binary's absolute path here and pass that to the daemon.
	target, err := os.Executable()
	if err != nil {
		// Fall back to /proc/self/exe; if the daemon reads it,
		// it will look in bin/bpfman and fail with a clear
		// "symbol not found" message. Better than panicking
		// during package init.
		target = "/proc/self/exe"
	}
	return typeSpec{
		name:     "uprobe",
		object:   "uprobe_counter.bpf.o",
		progName: "uprobe_counter",
		enumType: pb.BpfmanProgramType_UPROBE,
		attachBuilder: constantAttachBuilder(&pb.AttachInfo{
			Info: &pb.AttachInfo_UprobeAttachInfo{
				UprobeAttachInfo: &pb.UprobeAttachInfo{
					Target: target,
					FnName: &fnName,
				},
			},
		}),
	}
}

// Network types: each spec's attachBuilder allocates its own
// veth pair via testnet.NewTestVethPair and captures the
// host-side interface name in the returned closure. The veth
// thus has goroutine lifetime, which keeps the kernel netif
// state stable across the goroutine's iterations and avoids
// contention on the XDP/TC dispatcher slot limit
// (MaxPrograms = 10) when many goroutines attach concurrently
// to the same interface. TCX doesn't have a dispatcher slot
// limit, but reuses the same shape for consistency.
// WithoutConnectivityWarmup is required at every callsite: the
// gRPC lifecycle test never puts a packet on the wire, and the
// default warmup ping does not scale to the goroutine counts
// the test targets.

func xdpSpec() typeSpec {
	return typeSpec{
		name:     "xdp",
		object:   "xdp_pass.bpf.o",
		progName: "pass",
		enumType: pb.BpfmanProgramType_XDP,
		attachBuilder: func(t *testing.T, _ int) func() *pb.AttachInfo {
			iface := testnet.NewTestVethPair(t, testnet.WithoutConnectivityWarmup()).A.Name
			return func() *pb.AttachInfo {
				return &pb.AttachInfo{Info: &pb.AttachInfo_XdpAttachInfo{
					XdpAttachInfo: &pb.XDPAttachInfo{
						Iface:    iface,
						Priority: 100,
					},
				}}
			}
		},
	}
}

func tcSpec() typeSpec {
	return typeSpec{
		name:     "tc",
		object:   "tc_counter.bpf.o",
		progName: "stats",
		enumType: pb.BpfmanProgramType_TC,
		attachBuilder: func(t *testing.T, _ int) func() *pb.AttachInfo {
			iface := testnet.NewTestVethPair(t, testnet.WithoutConnectivityWarmup()).A.Name
			return func() *pb.AttachInfo {
				return &pb.AttachInfo{Info: &pb.AttachInfo_TcAttachInfo{
					TcAttachInfo: &pb.TCAttachInfo{
						Iface:     iface,
						Direction: "ingress",
						Priority:  100,
					},
				}}
			}
		},
	}
}

func tcxSpec() typeSpec {
	return typeSpec{
		name:     "tcx",
		object:   "tcx_counter.bpf.o",
		progName: "tcx_stats",
		enumType: pb.BpfmanProgramType_TCX,
		attachBuilder: func(t *testing.T, _ int) func() *pb.AttachInfo {
			iface := testnet.NewTestVethPair(t, testnet.WithoutConnectivityWarmup()).A.Name
			return func() *pb.AttachInfo {
				return &pb.AttachInfo{Info: &pb.AttachInfo_TcxAttachInfo{
					TcxAttachInfo: &pb.TCXAttachInfo{
						Iface:     iface,
						Direction: "ingress",
						Priority:  100,
					},
				}}
			}
		},
	}
}
