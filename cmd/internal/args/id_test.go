package args

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/kernel"
)

func TestParseProgramID_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  kernel.ProgramID
	}{
		{"42", 42},
		{"0x2a", 42},
		{"0X2A", 42},
		{"program/42", 42},
		{"  42  ", 42},
		{"0", 0},
		{"4294967295", 4294967295}, // max uint32
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseProgramID(tt.input)
			require.NoErrorf(t, err, "ParseProgramID(%q)", tt.input)
			assert.Equal(t, tt.want, got.Value)
		})
	}
}

func TestParseProgramID_Invalid(t *testing.T) {
	t.Parallel()

	// empty, blank, non-numeric, negative, above uint32, bad hex.
	for _, input := range []string{"", "   ", "garbage", "-1", "4294967296", "0x", "0xZZ", "1.5"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			_, err := ParseProgramID(input)
			assert.Errorf(t, err, "ParseProgramID(%q) should be rejected", input)
		})
	}
}

func TestParseLinkID_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  bpfman.LinkID
	}{
		{"42", 42},
		{"0x2a", 42},
		{"link/42", 42},
		{"0", 0},
		{"18446744073709551615", 18446744073709551615}, // max uint64
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLinkID(tt.input)
			require.NoErrorf(t, err, "ParseLinkID(%q)", tt.input)
			assert.Equal(t, tt.want, got.Value)
		})
	}
}

func TestParseLinkID_Invalid(t *testing.T) {
	t.Parallel()

	// empty, non-numeric, negative, above uint64, bad hex.
	for _, input := range []string{"", "garbage", "-1", "18446744073709551616", "0xZZ"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			_, err := ParseLinkID(input)
			assert.Errorf(t, err, "ParseLinkID(%q) should be rejected", input)
		})
	}
}

func TestParseObjectPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "prog.o")
	require.NoError(t, os.WriteFile(file, []byte("\x7fELF"), 0o644))

	got, err := ParseObjectPath(file)
	require.NoError(t, err, "an existing regular file is accepted")
	assert.Equal(t, file, got.Path)

	_, err = ParseObjectPath("")
	assert.Error(t, err, "empty path is rejected")

	_, err = ParseObjectPath(filepath.Join(dir, "does-not-exist.o"))
	assert.Error(t, err, "a non-existent path is rejected")

	_, err = ParseObjectPath(dir)
	assert.Error(t, err, "a directory is rejected")
}
