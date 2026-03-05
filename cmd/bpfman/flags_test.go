package main

import (
	"testing"
)

func TestOutputFlags_Format(t *testing.T) {
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
			name:   "wide",
			output: "wide",
			want:   OutputFormatWide,
		},
		{
			name:   "tree",
			output: "tree",
			want:   OutputFormatTree,
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
			f := &OutputFlags{Output: OutputValue{Value: tt.output}}
			got := f.JSONPathExpr()
			if got != tt.want {
				t.Errorf("JSONPathExpr() = %q, want %q", got, tt.want)
			}
		})
	}
}
