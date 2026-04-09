package ebpf_test

import (
	"path/filepath"
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/platform/ebpf"
)

// testObjectPath returns the path to a BPF object file built by
// make bpf-build. The tests require the BPF objects to be present;
// run make bpf-build (or make) first.
func testObjectPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "e2e", "testdata", "xdp_pass.bpf.o")
	return path
}

func TestDiscoverPrograms(t *testing.T) {
	programs, err := ebpf.DiscoverPrograms(testObjectPath(t))
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
		if prog.Type == (bpfman.ProgramType{}) {
			t.Errorf("program %q has unspecified type", prog.Name)
		}
		// fentry/fexit should be filtered out
		if prog.Type == bpfman.ProgramTypeFentry || prog.Type == bpfman.ProgramTypeFexit {
			t.Errorf("program %q is fentry/fexit but should have been filtered", prog.Name)
		}
	}
}

func TestDiscoverPrograms_NonExistentFile(t *testing.T) {
	_, err := ebpf.DiscoverPrograms("/nonexistent/path/to/file.o")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestValidatePrograms(t *testing.T) {
	objectPath := testObjectPath(t)

	// First discover what programs are available
	discovered, err := ebpf.DiscoverPrograms(objectPath)
	if err != nil {
		t.Fatalf("DiscoverPrograms failed: %v", err)
	}
	if len(discovered) == 0 {
		t.Fatal("no programs discovered in test file")
	}

	t.Run("valid programs", func(t *testing.T) {
		// Use actual program names from the object file
		names := make([]string, len(discovered))
		for i, d := range discovered {
			names[i] = d.Name
		}
		err := ebpf.ValidatePrograms(objectPath, names)
		if err != nil {
			t.Errorf("ValidatePrograms failed for valid programs: %v", err)
		}
	})

	t.Run("missing program", func(t *testing.T) {
		err := ebpf.ValidatePrograms(objectPath, []string{"nonexistent_program_xyz"})
		if err == nil {
			t.Error("expected error for missing program")
		}
	})

	t.Run("mix of valid and invalid", func(t *testing.T) {
		names := []string{discovered[0].Name, "nonexistent_program_xyz"}
		err := ebpf.ValidatePrograms(objectPath, names)
		if err == nil {
			t.Error("expected error for mixed valid/invalid programs")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		err := ebpf.ValidatePrograms(objectPath, []string{})
		if err != nil {
			t.Errorf("expected no error for empty list: %v", err)
		}
	})

	t.Run("nil list", func(t *testing.T) {
		err := ebpf.ValidatePrograms(objectPath, nil)
		if err != nil {
			t.Errorf("expected no error for nil list: %v", err)
		}
	})
}

func TestExtractAttachFunc(t *testing.T) {
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
			got := ebpf.ExtractAttachFunc(tc.section)
			if got != tc.expected {
				t.Errorf("ExtractAttachFunc(%q) = %q, want %q", tc.section, got, tc.expected)
			}
		})
	}
}

func TestInferProgramType(t *testing.T) {
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
		{"unknown_section", bpfman.ProgramType{}},
	}

	for _, tc := range tests {
		t.Run(tc.section, func(t *testing.T) {
			got := ebpf.InferProgramType(tc.section)
			if got != tc.expected {
				t.Errorf("InferProgramType(%q) = %v, want %v", tc.section, got, tc.expected)
			}
		})
	}
}
