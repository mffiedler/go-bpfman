package ebpf

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
)

// xdpPassGlobalsObject embeds the same xdp_pass object the external
// discover_test uses; this copy lives in the internal test package
// so the global-data tests can reach the unexported applyGlobalData.
//
//go:embed xdp_pass.bpf.o
var xdpPassGlobalsObject []byte

// xdpPassSpec parses the embedded xdp_pass object into a fresh
// CollectionSpec. xdp_pass.bpf.c declares two globals, config_u8
// (1 byte) and config_u32 (4 bytes), which the global-data tests
// target. Each call returns a new spec so t.Parallel callers do
// not share mutable state.
func xdpPassSpec(t *testing.T) *ebpf.CollectionSpec {
	t.Helper()
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(xdpPassGlobalsObject))
	if err != nil {
		t.Fatalf("load collection spec: %v", err)
	}
	return spec
}

// TestApplyGlobalData_UnknownKeyRejected pins finding 6: a
// global-data key that names no variable in the object must fail
// the load, mirroring Rust's must_exist=true (aya's
// ParseError::SymbolNotFound). The previous behaviour silently
// skipped the key, so a typo'd --global-data name loaded with the
// compile-time default.
func TestApplyGlobalData_UnknownKeyRejected(t *testing.T) {
	t.Parallel()

	err := applyGlobalData(xdpPassSpec(t), map[string][]byte{
		"config_u32":       {1, 0, 0, 0},
		"definitely_bogus": {0, 0, 0, 0},
	})
	if err == nil {
		t.Fatal("unknown global-data key must fail the load")
	}
	if !strings.Contains(err.Error(), "definitely_bogus") {
		t.Fatalf("error must name the unknown key, got: %v", err)
	}
}

// TestApplyGlobalData_KnownKeySucceeds is the happy-path
// regression: a key that names a real variable, sized to match, is
// applied without error.
func TestApplyGlobalData_KnownKeySucceeds(t *testing.T) {
	t.Parallel()

	if err := applyGlobalData(xdpPassSpec(t), map[string][]byte{
		"config_u8":  {7},
		"config_u32": {9, 0, 0, 0},
	}); err != nil {
		t.Fatalf("valid global data must apply cleanly: %v", err)
	}
}

// TestApplyGlobalData_WrongSizeRejected pins the size check Rust
// also enforces (aya's ParseError::InvalidGlobalData): a value
// whose length does not match the variable's size fails. cilium's
// VariableSpec.Set already enforces this; the test guards against a
// future refactor dropping the check.
func TestApplyGlobalData_WrongSizeRejected(t *testing.T) {
	t.Parallel()

	err := applyGlobalData(xdpPassSpec(t), map[string][]byte{
		"config_u32": {1}, // 1 byte for a 4-byte variable
	})
	if err == nil {
		t.Fatal("wrong-sized global data must fail the load")
	}
	if !strings.Contains(err.Error(), "config_u32") {
		t.Fatalf("error must name the variable, got: %v", err)
	}
}

// TestApplyGlobalData_EmptyIsNoOp confirms no globals is not an
// error.
func TestApplyGlobalData_EmptyIsNoOp(t *testing.T) {
	t.Parallel()

	if err := applyGlobalData(xdpPassSpec(t), nil); err != nil {
		t.Fatalf("empty global data must be a no-op, got: %v", err)
	}
}
