package cli

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
			name:   "custom-columns",
			output: "custom-columns=ID:.spec.kernel_id",
			want:   OutputFormatCustomColumns,
		},
		{
			name:   "custom-columns-file",
			output: "custom-columns-file=/path/to/file.txt",
			want:   OutputFormatCustomColumnsFile,
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
			name:    "partial custom-columns",
			output:  "custom-column",
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

func TestOutputFlags_CustomColumnsSpec(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "valid spec",
			output: "custom-columns=ID:.spec.kernel_id,NAME:.spec.meta.name",
			want:   "ID:.spec.kernel_id,NAME:.spec.meta.name",
		},
		{
			name:   "not custom-columns",
			output: "table",
			want:   "",
		},
		{
			name:   "empty value after equals",
			output: "custom-columns=",
			want:   "",
		},
		{
			name:   "custom-columns-file is not custom-columns",
			output: "custom-columns-file=/path/to/file",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &OutputFlags{Output: OutputValue{Value: tt.output}}
			got := f.CustomColumnsSpec()
			if got != tt.want {
				t.Errorf("CustomColumnsSpec() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutputFlags_CustomColumnsFile(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "valid path",
			output: "custom-columns-file=/path/to/columns.txt",
			want:   "/path/to/columns.txt",
		},
		{
			name:   "not custom-columns-file",
			output: "table",
			want:   "",
		},
		{
			name:   "empty value after equals",
			output: "custom-columns-file=",
			want:   "",
		},
		{
			name:   "custom-columns is not custom-columns-file",
			output: "custom-columns=ID:.spec.kernel_id",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &OutputFlags{Output: OutputValue{Value: tt.output}}
			got := f.CustomColumnsFile()
			if got != tt.want {
				t.Errorf("CustomColumnsFile() = %q, want %q", got, tt.want)
			}
		})
	}
}
