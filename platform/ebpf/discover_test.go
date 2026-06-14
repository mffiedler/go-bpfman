package ebpf_test

import (
	"bytes"
	_ "embed"
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/platform/ebpf"
)

// xdpPassObject is the compiled xdp_pass BPF object embedded at
// build time. The Makefile rule
// `platform/ebpf/xdp_pass.bpf.o: e2e/testdata/bpf/xdp_pass.bpf.c`
// emits the object next to this test file so go:embed can pick
// it up without reaching across packages.
//
//go:embed xdp_pass.bpf.o
var xdpPassObject []byte

// xdpPassReader returns a fresh io.ReaderAt over the embedded
// xdp_pass BPF object. Each call hands back a new bytes.Reader so
// concurrent (t.Parallel) callers don't share read state.
func xdpPassReader() *bytes.Reader {
	return bytes.NewReader(xdpPassObject)
}

func TestDiscoverPrograms(t *testing.T) {
	t.Parallel()

	programs, err := ebpf.DiscoverProgramsFromReader(xdpPassReader())
	if err != nil {
		t.Fatalf("DiscoverPrograms failed: %v", err)
	}

	if len(programs) == 0 {
		t.Fatal("expected at least one program to be discovered")
	}

	// Verify programs are sorted by name
	for i := 1; i < len(programs); i++ {
		if programs[i-1].Name >= programs[i].Name {
			t.Errorf("programs not sorted: %s >= %s", programs[i-1].Name, programs[i].Name)
		}
	}

	// Verify each program has required fields
	for _, prog := range programs {
		if prog.Name == "" {
			t.Error("program has empty name")
		}
		if prog.SectionName == "" {
			t.Error("program has empty section name")
		}
		if !prog.Type.Valid() {
			t.Errorf("program %q has unspecified type", prog.Name)
		}
		// fentry/fexit should be filtered out
		if prog.Type == bpfman.ProgramTypeFentry || prog.Type == bpfman.ProgramTypeFexit {
			t.Errorf("program %q is fentry/fexit but should have been filtered", prog.Name)
		}
	}
}

func TestDiscoverPrograms_NonExistentFile(t *testing.T) {
	t.Parallel()

	_, err := ebpf.DiscoverPrograms("/nonexistent/path/to/file.o")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestValidatePrograms(t *testing.T) {
	t.Parallel()

	// First discover what programs are available
	discovered, err := ebpf.DiscoverProgramsFromReader(xdpPassReader())
	if err != nil {
		t.Fatalf("DiscoverPrograms failed: %v", err)
	}
	if len(discovered) == 0 {
		t.Fatal("no programs discovered in test file")
	}

	t.Run("valid programs", func(t *testing.T) {
		t.Parallel()
		// Use actual program names from the object file
		names := make([]string, len(discovered))
		for i, d := range discovered {
			names[i] = d.Name
		}
		err := ebpf.ValidateProgramsFromReader(xdpPassReader(), names)
		if err != nil {
			t.Errorf("ValidatePrograms failed for valid programs: %v", err)
		}
	})

	t.Run("missing program", func(t *testing.T) {
		t.Parallel()
		err := ebpf.ValidateProgramsFromReader(xdpPassReader(), []string{"nonexistent_program_xyz"})
		if err == nil {
			t.Error("expected error for missing program")
		}
	})

	t.Run("mix of valid and invalid", func(t *testing.T) {
		t.Parallel()
		names := []string{discovered[0].Name, "nonexistent_program_xyz"}
		err := ebpf.ValidateProgramsFromReader(xdpPassReader(), names)
		if err == nil {
			t.Error("expected error for mixed valid/invalid programs")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()
		err := ebpf.ValidateProgramsFromReader(xdpPassReader(), []string{})
		if err != nil {
			t.Errorf("expected no error for empty list: %v", err)
		}
	})

	t.Run("nil list", func(t *testing.T) {
		t.Parallel()
		err := ebpf.ValidateProgramsFromReader(xdpPassReader(), nil)
		if err != nil {
			t.Errorf("expected no error for nil list: %v", err)
		}
	})
}

func TestExtractAttachFunc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		section  string
		expected string
	}{
		{"fentry/vfs_read", "vfs_read"},
		{"fexit/vfs_write", "vfs_write"},
		{"?fentry/do_sys_open", "do_sys_open"},
		{"kprobe/sys_open", "sys_open"},
		{"tracepoint/syscalls/sys_enter_read", "syscalls/sys_enter_read"},
		{"xdp", ""},
		{"tc", ""},
	}

	for _, tc := range tests {
		t.Run(tc.section, func(t *testing.T) {
			t.Parallel()
			got := ebpf.ExtractAttachFunc(tc.section)
			if got != tc.expected {
				t.Errorf("ExtractAttachFunc(%q) = %q, want %q", tc.section, got, tc.expected)
			}
		})
	}
}

func TestInferProgramType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		section  string
		expected bpfman.ProgramType
	}{
		{"kprobe/sys_open", bpfman.ProgramTypeKprobe},
		{"kprobe.multi/foo", bpfman.ProgramTypeKprobe},
		{"kretprobe/sys_open", bpfman.ProgramTypeKretprobe},
		{"uprobe/func", bpfman.ProgramTypeUprobe},
		{"uretprobe/func", bpfman.ProgramTypeUretprobe},
		{"tracepoint/syscalls/sys_enter_open", bpfman.ProgramTypeTracepoint},
		{"xdp", bpfman.ProgramTypeXDP},
		{"xdp.frags", bpfman.ProgramTypeXDP},
		{"tc", bpfman.ProgramTypeTC},
		{"classifier/ingress", bpfman.ProgramTypeTC},
		{"tcx/ingress", bpfman.ProgramTypeTCX},
		{"fentry/vfs_read", bpfman.ProgramTypeFentry},
		{"fexit/vfs_read", bpfman.ProgramTypeFexit},
		{"?kprobe/sys_open", bpfman.ProgramTypeKprobe}, // optional prefix
		{"unknown_section", ""},
	}

	for _, tc := range tests {
		t.Run(tc.section, func(t *testing.T) {
			t.Parallel()
			got := ebpf.InferProgramType(tc.section)
			if got != tc.expected {
				t.Errorf("InferProgramType(%q) = %v, want %v", tc.section, got, tc.expected)
			}
		})
	}
}
