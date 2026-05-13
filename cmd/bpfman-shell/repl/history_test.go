package repl

import "testing"

func TestCanonicaliseHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "bpfman program list", "bpfman program list"},
		{"empty", "", ""},
		{"whitespace only", "   \n  \t", ""},
		{
			name: "backslash continuation",
			in:   "bpfman program load file \\\n    --path foo.o \\\n    --programs tracepoint:kr",
			want: "bpfman program load file --path foo.o --programs tracepoint:kr",
		},
		{
			name: "let with bracket continuation",
			in:   "let prog <- bpfman program load file \\\n    --path foo.o \\\n    --programs tracepoint:kr",
			want: "let prog <- bpfman program load file --path foo.o --programs tracepoint:kr",
		},
		{
			name: "if block",
			in:   "if true {\n    print yes\n}",
			want: "if true { print yes }",
		},
		{
			name: "preserves quoted newlines",
			in:   "print \"line1\nline2\"",
			want: "print \"line1\nline2\"",
		},
		{
			name: "strips line comments",
			in:   "bpfman program list # all programs\nfoo",
			want: "bpfman program list foo",
		},
		{
			name: "hash inside quotes preserved",
			in:   "print \"#not a comment\"",
			want: "print \"#not a comment\"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CanonicaliseHistory(tc.in)
			if got != tc.want {
				t.Errorf("CanonicaliseHistory(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}
