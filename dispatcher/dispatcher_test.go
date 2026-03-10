package dispatcher_test

import (
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
)

func TestLoadXDPDispatcher(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	// Create config for 1 program
	cfg, err := dispatcher.NewXDPConfig(1)
	if err != nil {
		t.Fatalf("NewXDPConfig: %v", err)
	}
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
	cfg, err := dispatcher.NewTCConfig(1)
	if err != nil {
		t.Fatalf("NewTCConfig: %v", err)
	}

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

func TestNewXDPConfig(t *testing.T) {
	t.Run("valid range", func(t *testing.T) {
		for n := 1; n <= dispatcher.MaxPrograms; n++ {
			cfg, err := dispatcher.NewXDPConfig(n)
			if err != nil {
				t.Fatalf("NewXDPConfig(%d): unexpected error: %v", n, err)
			}
			if int(cfg.NumProgsEnabled) != n {
				t.Errorf("NewXDPConfig(%d): NumProgsEnabled = %d", n, cfg.NumProgsEnabled)
			}
		}
	})

	t.Run("default priorities", func(t *testing.T) {
		cfg, err := dispatcher.NewXDPConfig(1)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < dispatcher.MaxPrograms; i++ {
			if cfg.RunPrios[i] != dispatcher.DefaultPriority {
				t.Errorf("RunPrios[%d] = %d, want %d", i, cfg.RunPrios[i], dispatcher.DefaultPriority)
			}
		}
	})

	t.Run("zero", func(t *testing.T) {
		if _, err := dispatcher.NewXDPConfig(0); err == nil {
			t.Error("NewXDPConfig(0): expected error")
		}
	})

	t.Run("negative", func(t *testing.T) {
		if _, err := dispatcher.NewXDPConfig(-1); err == nil {
			t.Error("NewXDPConfig(-1): expected error")
		}
	})

	t.Run("exceeds max", func(t *testing.T) {
		if _, err := dispatcher.NewXDPConfig(dispatcher.MaxPrograms + 1); err == nil {
			t.Errorf("NewXDPConfig(%d): expected error", dispatcher.MaxPrograms+1)
		}
	})
}

func TestNewTCConfig(t *testing.T) {
	t.Run("valid range", func(t *testing.T) {
		for n := 1; n <= dispatcher.MaxPrograms; n++ {
			cfg, err := dispatcher.NewTCConfig(n)
			if err != nil {
				t.Fatalf("NewTCConfig(%d): unexpected error: %v", n, err)
			}
			if int(cfg.NumProgsEnabled) != n {
				t.Errorf("NewTCConfig(%d): NumProgsEnabled = %d", n, cfg.NumProgsEnabled)
			}
		}
	})

	t.Run("default priorities", func(t *testing.T) {
		cfg, err := dispatcher.NewTCConfig(1)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < dispatcher.MaxPrograms; i++ {
			if cfg.RunPrios[i] != dispatcher.DefaultPriority {
				t.Errorf("RunPrios[%d] = %d, want %d", i, cfg.RunPrios[i], dispatcher.DefaultPriority)
			}
		}
	})

	t.Run("zero", func(t *testing.T) {
		if _, err := dispatcher.NewTCConfig(0); err == nil {
			t.Error("NewTCConfig(0): expected error")
		}
	})

	t.Run("negative", func(t *testing.T) {
		if _, err := dispatcher.NewTCConfig(-1); err == nil {
			t.Error("NewTCConfig(-1): expected error")
		}
	})

	t.Run("exceeds max", func(t *testing.T) {
		if _, err := dispatcher.NewTCConfig(dispatcher.MaxPrograms + 1); err == nil {
			t.Errorf("NewTCConfig(%d): expected error", dispatcher.MaxPrograms+1)
		}
	})
}

func TestSlotName(t *testing.T) {
	t.Run("valid positions", func(t *testing.T) {
		for i := 0; i < dispatcher.MaxPrograms; i++ {
			name, err := dispatcher.SlotName(i)
			if err != nil {
				t.Fatalf("SlotName(%d): unexpected error: %v", i, err)
			}
			want := "prog" + string(rune('0'+i))
			if name != want {
				t.Errorf("SlotName(%d) = %q, want %q", i, name, want)
			}
		}
	})

	t.Run("negative", func(t *testing.T) {
		if _, err := dispatcher.SlotName(-1); err == nil {
			t.Error("SlotName(-1): expected error")
		}
	})

	t.Run("at max", func(t *testing.T) {
		if _, err := dispatcher.SlotName(dispatcher.MaxPrograms); err == nil {
			t.Errorf("SlotName(%d): expected error", dispatcher.MaxPrograms)
		}
	})

	t.Run("above max", func(t *testing.T) {
		if _, err := dispatcher.SlotName(100); err == nil {
			t.Error("SlotName(100): expected error")
		}
	})
}

func TestProceedOnMask(t *testing.T) {
	tests := []struct {
		name    string
		actions []dispatcher.XDPAction
		want    uint32
	}{
		{"empty", nil, 0},
		{"pass only", []dispatcher.XDPAction{dispatcher.XDPPass}, 1 << 2},
		{"drop only", []dispatcher.XDPAction{dispatcher.XDPDrop}, 1 << 1},
		{"pass and drop", []dispatcher.XDPAction{dispatcher.XDPPass, dispatcher.XDPDrop}, (1 << 2) | (1 << 1)},
		{"all actions", []dispatcher.XDPAction{
			dispatcher.XDPAborted, dispatcher.XDPDrop, dispatcher.XDPPass,
			dispatcher.XDPTX, dispatcher.XDPRedirect,
		}, (1 << 0) | (1 << 1) | (1 << 2) | (1 << 3) | (1 << 4)},
		{"duplicate", []dispatcher.XDPAction{dispatcher.XDPPass, dispatcher.XDPPass}, 1 << 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dispatcher.ProceedOnMask(tt.actions...)
			if got != tt.want {
				t.Errorf("ProceedOnMask(%v) = 0x%x, want 0x%x", tt.actions, got, tt.want)
			}
		})
	}
}

func TestParseDispatcherType(t *testing.T) {
	tests := []struct {
		input   string
		want    dispatcher.DispatcherType
		wantErr bool
	}{
		{"xdp", dispatcher.DispatcherTypeXDP, false},
		{"tc-ingress", dispatcher.DispatcherTypeTCIngress, false},
		{"tc-egress", dispatcher.DispatcherTypeTCEgress, false},
		{"", dispatcher.DispatcherType{}, true},
		{"XDP", dispatcher.DispatcherType{}, true},
		{"tc_ingress", dispatcher.DispatcherType{}, true},
		{"unknown", dispatcher.DispatcherType{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := dispatcher.ParseDispatcherType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDispatcherType(%q): err = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseDispatcherType(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnmarshalText(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		var dt dispatcher.DispatcherType
		if err := dt.UnmarshalText([]byte("xdp")); err != nil {
			t.Fatalf("UnmarshalText(xdp): %v", err)
		}
		if dt != dispatcher.DispatcherTypeXDP {
			t.Errorf("got %v, want %v", dt, dispatcher.DispatcherTypeXDP)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		var dt dispatcher.DispatcherType
		if err := dt.UnmarshalText([]byte("bogus")); err == nil {
			t.Error("UnmarshalText(bogus): expected error")
		}
	})
}

func TestChainCallShift(t *testing.T) {
	tests := []struct {
		dt   dispatcher.DispatcherType
		want uint
	}{
		{dispatcher.DispatcherTypeXDP, 0},
		{dispatcher.DispatcherTypeTCIngress, 1},
		{dispatcher.DispatcherTypeTCEgress, 1},
	}
	for _, tt := range tests {
		t.Run(tt.dt.String(), func(t *testing.T) {
			if got := tt.dt.ChainCallShift(); got != tt.want {
				t.Errorf("%s.ChainCallShift() = %d, want %d", tt.dt, got, tt.want)
			}
		})
	}
}

func TestTCParentHandle(t *testing.T) {
	tests := []struct {
		dt   dispatcher.DispatcherType
		want uint32
	}{
		{dispatcher.DispatcherTypeTCIngress, netlink.HANDLE_MIN_INGRESS},
		{dispatcher.DispatcherTypeTCEgress, netlink.HANDLE_MIN_EGRESS},
		{dispatcher.DispatcherTypeXDP, 0},
	}
	for _, tt := range tests {
		t.Run(tt.dt.String(), func(t *testing.T) {
			if got := dispatcher.TCParentHandle(tt.dt); got != tt.want {
				t.Errorf("TCParentHandle(%s) = 0x%x, want 0x%x", tt.dt, got, tt.want)
			}
		})
	}
}

func TestXDPDispatcherAttachSpecValidate(t *testing.T) {
	valid := dispatcher.XDPDispatcherAttachSpec{
		Target:      bpfman.AttachTarget{IfIndex: 1},
		ProgPinPath: "/some/path",
		LinkPinPath: "/some/link",
		NumProgs:    1,
	}

	t.Run("valid", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("max progs", func(t *testing.T) {
		s := valid
		s.NumProgs = dispatcher.MaxPrograms
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("zero progs", func(t *testing.T) {
		s := valid
		s.NumProgs = 0
		if err := s.Validate(); err == nil {
			t.Error("expected error for NumProgs=0")
		}
	})

	t.Run("exceeds max progs", func(t *testing.T) {
		s := valid
		s.NumProgs = dispatcher.MaxPrograms + 1
		if err := s.Validate(); err == nil {
			t.Error("expected error for NumProgs > MaxPrograms")
		}
	})

	t.Run("negative progs", func(t *testing.T) {
		s := valid
		s.NumProgs = -1
		if err := s.Validate(); err == nil {
			t.Error("expected error for NumProgs=-1")
		}
	})
}

func TestTCDispatcherAttachSpecValidate(t *testing.T) {
	valid := dispatcher.TCDispatcherAttachSpec{
		Target:      bpfman.AttachTarget{IfIndex: 1},
		IfName:      "lo",
		ProgPinPath: "/some/path",
		Direction:   bpfman.TCDirectionIngress,
		NumProgs:    1,
	}

	t.Run("valid", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("max progs", func(t *testing.T) {
		s := valid
		s.NumProgs = dispatcher.MaxPrograms
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("zero progs", func(t *testing.T) {
		s := valid
		s.NumProgs = 0
		if err := s.Validate(); err == nil {
			t.Error("expected error for NumProgs=0")
		}
	})

	t.Run("exceeds max progs", func(t *testing.T) {
		s := valid
		s.NumProgs = dispatcher.MaxPrograms + 1
		if err := s.Validate(); err == nil {
			t.Error("expected error for NumProgs > MaxPrograms")
		}
	})
}

func TestXDPExtensionAttachSpecValidate(t *testing.T) {
	valid := dispatcher.XDPExtensionAttachSpec{
		DispatcherPinPath: "/disp",
		ProgPinPath:       "/prog",
		ProgramName:       "test",
		Position:          0,
	}

	t.Run("valid at 0", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid at max-1", func(t *testing.T) {
		s := valid
		s.Position = dispatcher.MaxPrograms - 1
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("negative position", func(t *testing.T) {
		s := valid
		s.Position = -1
		if err := s.Validate(); err == nil {
			t.Error("expected error for Position=-1")
		}
	})

	t.Run("position at max", func(t *testing.T) {
		s := valid
		s.Position = dispatcher.MaxPrograms
		if err := s.Validate(); err == nil {
			t.Error("expected error for Position=MaxPrograms")
		}
	})
}

func TestTCExtensionAttachSpecValidate(t *testing.T) {
	valid := dispatcher.TCExtensionAttachSpec{
		DispatcherPinPath: "/disp",
		ProgPinPath:       "/prog",
		ProgramName:       "test",
		Position:          0,
	}

	t.Run("valid at 0", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("negative position", func(t *testing.T) {
		s := valid
		s.Position = -1
		if err := s.Validate(); err == nil {
			t.Error("expected error for Position=-1")
		}
	})

	t.Run("position at max", func(t *testing.T) {
		s := valid
		s.Position = dispatcher.MaxPrograms
		if err := s.Validate(); err == nil {
			t.Error("expected error for Position=MaxPrograms")
		}
	})
}
