package driver

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatInput_CanonicalisesSourceWithoutImportExpansion(t *testing.T) {
	t.Parallel()

	src := "# Header prose.\n\nimport ./lib.bpfman\nif not (null $x) { print ($x + 1) }\n"
	r := NewScannerReader(strings.NewReader(src), nil)
	var out, errOut bytes.Buffer

	if FormatInput(r, &out, &errOut, "main.bpfman") {
		t.Fatalf("FormatInput reported issue: %s", errOut.String())
	}

	want := "# Header prose.\n\nimport ./lib.bpfman\nif not null $x {\n    print ($x + 1)\n}\n"
	if got := out.String(); got != want {
		t.Fatalf("FormatInput() =\n%s\nwant:\n%s", got, want)
	}
}
