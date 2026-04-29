package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
)

func TestListProgramsCmd_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attached   bool
		unattached bool
		wantErr    bool
		errMsg     string
	}{
		{
			name:       "neither flag is valid",
			attached:   false,
			unattached: false,
			wantErr:    false,
		},
		{
			name:       "only attached is valid",
			attached:   true,
			unattached: false,
			wantErr:    false,
		},
		{
			name:       "only unattached is valid",
			attached:   false,
			unattached: true,
			wantErr:    false,
		},
		{
			name:       "both flags is invalid",
			attached:   true,
			unattached: true,
			wantErr:    true,
			errMsg:     "mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := &ListProgramsCmd{
				Attached:   tt.attached,
				Unattached: tt.unattached,
			}
			err := cmd.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestParseProgramTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []string
		expected map[bpfman.ProgramType]struct{}
		wantErr  bool
		errMsg   string
	}{
		{
			name:     "empty input",
			input:    []string{},
			expected: map[bpfman.ProgramType]struct{}{},
			wantErr:  false,
		},
		{
			name:     "single type lowercase",
			input:    []string{"xdp"},
			expected: map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}},
			wantErr:  false,
		},
		{
			name:     "single type uppercase",
			input:    []string{"XDP"},
			expected: map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}},
			wantErr:  false,
		},
		{
			name:     "single type mixed case",
			input:    []string{"Xdp"},
			expected: map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}},
			wantErr:  false,
		},
		{
			name:  "multiple types",
			input: []string{"xdp", "kprobe", "tc"},
			expected: map[bpfman.ProgramType]struct{}{
				bpfman.ProgramTypeXDP:    {},
				bpfman.ProgramTypeKprobe: {},
				bpfman.ProgramTypeTC:     {},
			},
			wantErr: false,
		},
		{
			name:  "types with whitespace",
			input: []string{"  xdp  ", "kprobe"},
			expected: map[bpfman.ProgramType]struct{}{
				bpfman.ProgramTypeXDP:    {},
				bpfman.ProgramTypeKprobe: {},
			},
			wantErr: false,
		},
		{
			name:     "empty strings are ignored",
			input:    []string{"xdp", "", "  ", "kprobe"},
			expected: map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}, bpfman.ProgramTypeKprobe: {}},
			wantErr:  false,
		},
		{
			name:    "invalid type",
			input:   []string{"invalid"},
			wantErr: true,
			errMsg:  `unknown program type "invalid"`,
		},
		{
			name:    "invalid type preserves original input in error",
			input:   []string{"INVALID"},
			wantErr: true,
			errMsg:  `unknown program type "INVALID"`,
		},
		{
			name:    "mixed valid and invalid",
			input:   []string{"xdp", "notreal"},
			wantErr: true,
			errMsg:  `unknown program type "notreal"`,
		},
		{
			name:    "error message shows valid types",
			input:   []string{"bad"},
			wantErr: true,
			errMsg:  "valid:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := ParseProgramTypes(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestParseProgramTypes_AllValidTypes(t *testing.T) {
	t.Parallel()

	// Verify all program type names can be parsed
	for _, name := range bpfman.ProgramTypeNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := ParseProgramTypes([]string{name})
			require.NoError(t, err)
			assert.Len(t, result, 1)
		})
	}
}

func TestParseProgramTypes_CaseInsensitivity(t *testing.T) {
	t.Parallel()

	// Test various case combinations
	cases := []string{"XDP", "xdp", "Xdp", "xDp", "xDP", "XdP"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			result, err := ParseProgramTypes([]string{c})
			require.NoError(t, err)
			_, ok := result[bpfman.ProgramTypeXDP]
			assert.True(t, ok, "expected XDP type for input %q", c)
		})
	}
}
