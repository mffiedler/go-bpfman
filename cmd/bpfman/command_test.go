package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/replang"
)

func TestParseShowProgram(t *testing.T) {
	structuredVal, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":42}}`))
	require.NoError(t, err)

	linkVal, err := replang.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        kernel.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	})
	require.NoError(t, err)

	tests := []struct {
		name       string
		args       []replang.Arg
		wantID     kernel.ProgramID
		wantView   string
		wantOutput string
		wantErr    string
	}{
		{
			name:       "numeric ID only",
			args:       []replang.Arg{replang.WordArg{Text: "123"}},
			wantID:     123,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name:       "hex ID",
			args:       []replang.Arg{replang.WordArg{Text: "0x1a"}},
			wantID:     26,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "structured variable ref",
			args: []replang.Arg{
				replang.StructuredValueArg{Name: "prog", Value: structuredVal},
			},
			wantID:     42,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "scalar value arg",
			args: []replang.Arg{
				replang.ScalarValueArg{Text: "55"},
			},
			wantID:     55,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "with view argument",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "links"},
			},
			wantID:     100,
			wantView:   "links",
			wantOutput: "table",
		},
		{
			name: "with output flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "json"},
			},
			wantID:     100,
			wantView:   "summary",
			wantOutput: "json",
		},
		{
			name: "view and output flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "maps"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "wide"},
			},
			wantID:     100,
			wantView:   "maps",
			wantOutput: "wide",
		},
		{
			name: "output flag before view",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "json"},
				replang.WordArg{Text: "paths"},
			},
			wantID:     100,
			wantView:   "paths",
			wantOutput: "json",
		},
		{
			name: "structured ref with view",
			args: []replang.Arg{
				replang.StructuredValueArg{Name: "prog", Value: structuredVal},
				replang.WordArg{Text: "maps"},
			},
			wantID:     42,
			wantView:   "maps",
			wantOutput: "table",
		},
		{
			name:    "no arguments",
			args:    []replang.Arg{},
			wantErr: "requires a program ID",
		},
		{
			name: "duplicate -o flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "json"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "wide"},
			},
			wantErr: "duplicate -o flag",
		},
		{
			name: "unknown flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "--verbose"},
			},
			wantErr: "unknown flag",
		},
		{
			name: "unknown view",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "nonsense"},
			},
			wantErr: "unknown view",
		},
		{
			name: "-o without value",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
			},
			wantErr: "-o requires a value",
		},
		{
			name: "wrong origin type on structured ref",
			args: []replang.Arg{
				replang.StructuredValueArg{Name: "mylink", Value: linkVal},
			},
			wantErr: "not a program",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := parseShowProgram(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, cmd.ID)
			assert.Equal(t, tt.wantView, cmd.View)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}
