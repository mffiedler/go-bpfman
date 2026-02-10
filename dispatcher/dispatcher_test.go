package dispatcher_test

import (
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman/dispatcher"
)

func TestLoadXDPDispatcher(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	// Create config for 1 program
	cfg := dispatcher.NewXDPConfig(1)
	cfg.ChainCallActions[0] = dispatcher.ProceedOnMask(dispatcher.XDPPass)

	// Load the dispatcher spec
	spec, err := dispatcher.LoadXDPDispatcher(cfg)
	if err != nil {
		t.Fatalf("LoadXDPDispatcher: %v", err)
	}

	t.Logf("Loaded XDP dispatcher spec with %d programs", len(spec.Programs))
	for name, prog := range spec.Programs {
		t.Logf("  Program: %s (type: %s)", name, prog.Type)
	}

	// Find the main dispatcher program
	dispatcherSpec, ok := spec.Programs["xdp_dispatcher"]
	if !ok {
		t.Fatal("xdp_dispatcher program not found in spec")
	}
	t.Logf("Dispatcher program type: %s", dispatcherSpec.Type)

	// Load the dispatcher into the kernel
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	defer coll.Close()

	prog := coll.Programs["xdp_dispatcher"]
	if prog == nil {
		t.Fatal("xdp_dispatcher not found in collection")
	}

	info, err := prog.Info()
	if err != nil {
		t.Fatalf("dispatcher.Info: %v", err)
	}
	id, _ := info.ID()
	t.Logf("Dispatcher loaded: ID=%d, Name=%s", id, info.Name)

	// Attach dispatcher to lo
	lo, err := link.AttachXDP(link.XDPOptions{
		Program:   prog,
		Interface: 1, // lo is always ifindex 1
	})
	if err != nil {
		t.Fatalf("AttachXDP to lo: %v", err)
	}
	defer lo.Close()

	t.Log("XDP dispatcher attached to lo")

	// List the stub programs that can be replaced
	for name, p := range spec.Programs {
		if name != "xdp_dispatcher" {
			t.Logf("  Stub function: %s (type: %s)", name, p.Type)
		}
	}
}

func TestLoadTCDispatcher(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	// Create config for 1 program
	cfg := dispatcher.NewTCConfig(1)

	// Load the dispatcher spec
	spec, err := dispatcher.LoadTCDispatcher(cfg)
	if err != nil {
		t.Fatalf("LoadTCDispatcher: %v", err)
	}

	t.Logf("Loaded TC dispatcher spec with %d programs", len(spec.Programs))
	for name, prog := range spec.Programs {
		t.Logf("  Program: %s (type: %s)", name, prog.Type)
	}
}
