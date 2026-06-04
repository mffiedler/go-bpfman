package main

import (
	"os"
	"path/filepath"
	"testing"
)

//nolint:paralleltest // mutates the os.Args process global via newCLIForArgs; cannot run in parallel.
func TestSelectedCommandAllowsRootless(t *testing.T) {
	for _, args := range [][]string{
		{"bpfman", "version"},
		{"bpfman", "image", "build", "example.test/x:latest", "x.o"},
		{"bpfman", "image", "build", "example.test/x:latest", "linux/amd64=x.o"},
		{"bpfman", "image", "generate-build-args", "x.o"},
		{"bpfman", "image", "generate-build-args", "linux/amd64=x.o"},
		{"bpfman", "image", "inspect", "example.test/x:latest"},
		{"bpfman", "image", "verify", "example.test/x:latest"},
		{
			"bpfman", "image", "verify", "example.test/x:latest",
			"--certificate-identity", "signer@example.com",
			"--certificate-oidc-issuer", "https://github.com/login/oauth",
		},
	} {
		t.Run(args[1], func(t *testing.T) {
			cli := newCLIForArgs(t, args)
			if !selectedCommandAllowsRootless(cli.kctx) {
				t.Fatalf("selectedCommandAllowsRootless(%v) = false, want true", args)
			}
		})
	}
}

//nolint:paralleltest // mutates the os.Args process global via newCLIForArgs; cannot run in parallel.
func TestSelectedCommandRequiresRootByDefault(t *testing.T) {
	for _, args := range [][]string{
		{"bpfman", "program", "load", "file", "--path", "x.o"},
		{"bpfman", "program", "load", "image", "--image-url", "example.test/x:latest"},
		{"bpfman", "program", "list"},
		{"bpfman", "link", "list"},
		{"bpfman", "serve"},
	} {
		t.Run(args[1], func(t *testing.T) {
			cli := newCLIForArgs(t, args)
			if selectedCommandAllowsRootless(cli.kctx) {
				t.Fatalf("selectedCommandAllowsRootless(%v) = true, want false", args)
			}
		})
	}
}

func newCLIForArgs(t *testing.T, args []string) *CLI {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "bpfman.toml")
	if err := os.WriteFile(configPath, nil, 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	args = append([]string{args[0], "--config", configPath}, args[1:]...)

	oldArgs := os.Args
	os.Args = append([]string(nil), args...)
	t.Cleanup(func() {
		os.Args = oldArgs
	})

	cli, err := NewCLI()
	if err != nil {
		t.Fatalf("NewCLI(%v): %v", args, err)
	}
	return cli
}
