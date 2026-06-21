package main

import (
	"testing"

	"github.com/frobware/go-bpfman/cmd/internal/args"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProgramDeleteCmd_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cmd     ProgramDeleteCmd
		wantErr string
	}{
		{
			name:    "neither --all nor IDs",
			cmd:     ProgramDeleteCmd{},
			wantErr: "provide at least one program ID or use --all",
		},
		{
			name: "both --all and IDs",
			cmd: ProgramDeleteCmd{
				All:        true,
				ProgramIDs: []args.ProgramID{{Value: 1}},
			},
			wantErr: "--all and explicit program IDs are mutually exclusive",
		},
		{
			name: "--all alone is valid",
			cmd:  ProgramDeleteCmd{All: true},
		},
		{
			name: "IDs alone is valid",
			cmd:  ProgramDeleteCmd{ProgramIDs: []args.ProgramID{{Value: 1}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cmd.Validate()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
