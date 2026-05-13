package repl

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

func TestArgTexts(t *testing.T) {
	t.Parallel()

	args := []shell.Arg{
		shell.WordArg{Text: "show"},
		shell.WordArg{Text: "program"},
		shell.ScalarValueArg{Text: "42"},
	}
	got := ArgTexts(args)
	assert.Equal(t, []string{"show", "program", "42"}, got)
}

func TestArgTexts_Empty(t *testing.T) {
	t.Parallel()

	got := ArgTexts(nil)
	assert.Empty(t, got)
}

func TestArgTexts_StructuredValueArg(t *testing.T) {
	t.Parallel()

	args := []shell.Arg{
		shell.WordArg{Text: "show"},
		shell.WordArg{Text: "program"},
		shell.StructuredValueArg{Name: "prog", Value: shell.ValueFromMap(map[string]any{"id": "42"})},
	}
	got := ArgTexts(args)
	assert.Equal(t, []string{"show", "program", "$prog"}, got)
}

func TestArgTexts_QuotedArg(t *testing.T) {
	t.Parallel()

	args := []shell.Arg{
		shell.WordArg{Text: "load"},
		shell.QuotedArg{Text: "my file.o"},
	}
	got := ArgTexts(args)
	assert.Equal(t, []string{"load", "my file.o"}, got)
}
