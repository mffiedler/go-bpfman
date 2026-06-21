package cliformat

import (
	"testing"
)

func TestOutputFlags_Format(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		output  string
		want    OutputFormat
		wantErr bool
	}{
		{
			name:   "table",
			output: "table",
			want:   OutputFormatTable,
		},
		{
			name:   "json",
			output: "json",
			want:   OutputFormatJSON,
		},
		{
			name:   "jsonpath",
			output: "jsonpath={.items}",
			want:   OutputFormatJSONPath,
		},
		{
			name:    "unknown format",
			output:  "xml",
			wantErr: true,
		},
		{
			name:    "empty",
			output:  "",
			wantErr: true,
		},
		{
			name:    "custom-columns no longer supported",
			output:  "custom-columns=ID:.record.program_id",
			wantErr: true,
		},
		{
			name:    "custom-columns-file no longer supported",
			output:  "custom-columns-file=/path/to/file.txt",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &OutputFlags{Output: OutputValue{Value: tt.output}}
			got, err := f.Format()
			if (err != nil) != tt.wantErr {
				t.Errorf("Format() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("Format() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutputFlags_JSONPathExpr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "jsonpath expression",
			output: "jsonpath={.items[*].metadata.name}",
			want:   "{.items[*].metadata.name}",
		},
		{
			name:   "not jsonpath",
			output: "table",
			want:   "",
		},
		{
			name:   "empty value after equals",
			output: "jsonpath=",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &OutputFlags{Output: OutputValue{Value: tt.output}}
			got := f.JSONPathExpr()
			if got != tt.want {
				t.Errorf("JSONPathExpr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutputFlags_NeedsLinkGetProgramName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		output  string
		want    bool
		wantErr bool
	}{
		{
			name:   "table",
			output: "table",
			want:   true,
		},
		{
			name:   "json",
			output: "json",
			want:   false,
		},
		{
			name:   "jsonpath",
			output: "jsonpath={.record.id}",
			want:   false,
		},
		{
			name:    "invalid format",
			output:  "nonsense",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &OutputFlags{Output: OutputValue{Value: tt.output}}
			got, err := f.NeedsLinkGetProgramName()
			if (err != nil) != tt.wantErr {
				t.Fatalf("NeedsLinkGetProgramName() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("NeedsLinkGetProgramName() = %v, want %v", got, tt.want)
			}
		})
	}
}
