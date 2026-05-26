package lower

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
)

// TestLower_RetryRejectedOutsidePollAndDef pins the defence-in-
// depth invariant the runtime relies on: a RetryStmt only makes
// sense inside a poll attempt's lexical body or inside a helper
// def that is callable from a poll attempt. The static checker
// normally catches a misplaced `retry`, but Lower is callable on
// any parsed program (the checker is not a precondition), and
// the emitted retry sequence pops an attempt frame plus drains
// attempt-local defers. If those structures do not exist the
// resulting IR will dismantle the program-level frame and the
// runtime cannot recover. The lowerer must reject the shape
// itself rather than trust an upstream gate.
func TestLower_RetryRejectedOutsidePollAndDef(t *testing.T) {
	t.Parallel()

	src := "retry \"top-level\""
	tokens, err := syntax.Tokenise(src)
	require.NoError(t, err)
	prog, err := syntax.Parse(tokens)
	require.NoError(t, err)

	_, err = Lower(prog)
	require.Error(t, err)
	assert.True(t,
		strings.Contains(err.Error(), "retry") &&
			(strings.Contains(err.Error(), "outside") || strings.Contains(err.Error(), "poll")),
		"expected the lowerer to reject a top-level retry, got %q", err.Error())
}
