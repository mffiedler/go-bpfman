package main

import (
	"debug/elf"
	"strings"
	"testing"

	"github.com/frobware/go-bpfman/internal/imagebuild"
)

func TestBytecodeSourceRejectsConflictingModes(t *testing.T) {
	t.Parallel()

	_, err := bytecodeSource([]string{"xdp_pass.bpf.o"}, "bpf2go")
	if err == nil {
		t.Fatal("bytecodeSource returned nil error for conflicting modes")
	}
	if !strings.Contains(err.Error(), "conflicts with bytecode inputs") {
		t.Fatalf("bytecodeSource error = %q, want conflict error", err)
	}
}

func TestImageBuildBytecodeSourceAcceptsSinglePositional(t *testing.T) {
	t.Parallel()

	source, err := bytecodeSource([]string{"xdp_pass.bpf.o"}, "")
	if err != nil {
		t.Fatalf("bytecodeSource returned error: %v", err)
	}
	plan, err := imagebuild.Build(source, func(path string, _ elf.Data) (imagebuild.Info, error) {
		if path != "xdp_pass.bpf.o" {
			t.Fatalf("path = %q, want xdp_pass.bpf.o", path)
		}
		return imagebuild.Info{}, nil
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	assertStringSliceEqual(t, plan.BuildArgs, []string{"BYTECODE_FILE=xdp_pass.bpf.o"})
}

func TestImageBuildBytecodeSourceRejectsMultipleBarePositionals(t *testing.T) {
	t.Parallel()

	_, err := bytecodeSource([]string{"a.bpf.o", "b.bpf.o"}, "")
	if err == nil {
		t.Fatal("bytecodeSource returned nil error for multiple bare positionals")
	}
	for _, want := range []string{"cannot infer OCI platforms", "EM_BPF", "linux/arch=BYTECODE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("bytecodeSource error = %q, want substring %q", err, want)
		}
	}
}

func TestBytecodeSourceAcceptsPlatformMappedInputs(t *testing.T) {
	t.Parallel()

	source, err := bytecodeSource([]string{
		"linux/amd64=bpf_x86_bpfel.o",
		"linux/s390x=bpf_s390_bpfeb.o",
	}, "")
	if err != nil {
		t.Fatalf("bytecodeSource returned error: %v", err)
	}
	plan, err := imagebuild.Build(source, func(path string, _ elf.Data) (imagebuild.Info, error) {
		return imagebuild.Info{}, nil
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	assertStringSliceEqual(t, plan.Platforms, []string{"linux/amd64", "linux/s390x"})
	assertStringSliceEqual(t, plan.BuildArgs, []string{
		"BC_AMD64_EL=bpf_x86_bpfel.o",
		"BC_S390X_EB=bpf_s390_bpfeb.o",
	})
}

func TestBytecodeSourceRejectsMixedBareAndMappedInputs(t *testing.T) {
	t.Parallel()

	_, err := bytecodeSource([]string{"xdp_pass.bpf.o", "linux/amd64=bpf_x86_bpfel.o"}, "")
	if err == nil {
		t.Fatal("bytecodeSource returned nil error for mixed inputs")
	}
	if !strings.Contains(err.Error(), "cannot mix bare bytecode inputs with platform-mapped inputs") {
		t.Fatalf("bytecodeSource error = %q, want mixed-input error", err)
	}
}

func TestBytecodeSourceRejectsUnknownMappedPlatform(t *testing.T) {
	t.Parallel()

	_, err := bytecodeSource([]string{"linux/sparc=bpf_sparc_bpfel.o"}, "")
	if err == nil {
		t.Fatal("bytecodeSource returned nil error for unsupported platform")
	}
	if !strings.Contains(err.Error(), `unsupported OCI platform "linux/sparc"`) {
		t.Fatalf("bytecodeSource error = %q, want unsupported-platform error", err)
	}
}

func TestBytecodeSourceRejectsDuplicateMappedPlatform(t *testing.T) {
	t.Parallel()

	_, err := bytecodeSource([]string{
		"linux/amd64=a.bpf.o",
		"linux/amd64=b.bpf.o",
	}, "")
	if err == nil {
		t.Fatal("bytecodeSource returned nil error for duplicate platform")
	}
	if !strings.Contains(err.Error(), "platform linux/amd64 specified more than once") {
		t.Fatalf("bytecodeSource error = %q, want duplicate-platform error", err)
	}
}

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d\ngot:  %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q\ngot:  %#v\nwant: %#v", i, got[i], want[i], got, want)
		}
	}
}
