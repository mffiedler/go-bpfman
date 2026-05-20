package shell

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryableViaErrorsAs is the lookup retrying constructs perform:
// walk the error chain via errors.As to find any value that
// implements RetryableError, then ask it. The helper centralises
// the call shape so the tests below all exercise the same
// classification path eventually will consume.
func retryableViaErrorsAs(err error) (RetryableError, bool) {
	var r RetryableError
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

func TestRetryable_GuardFailure(t *testing.T) {
	t.Parallel()

	err := error(&GuardFailure{Primary: "p", Envelope: Envelope{OK: false, Code: 1}})
	r, ok := retryableViaErrorsAs(err)
	require.True(t, ok, "GuardFailure must satisfy RetryableError")
	assert.True(t, r.Retryable(), "GuardFailure is retryable")
}

func TestRetryable_CommandFailure(t *testing.T) {
	t.Parallel()

	err := error(&CommandFailure{Envelope: Envelope{OK: false, Code: 2}})
	r, ok := retryableViaErrorsAs(err)
	require.True(t, ok, "CommandFailure must satisfy RetryableError")
	assert.True(t, r.Retryable())
}

func TestRetryable_AssertFailure(t *testing.T) {
	t.Parallel()

	err := error(&AssertFailure{Expr: "x == 1"})
	r, ok := retryableViaErrorsAs(err)
	require.True(t, ok, "AssertFailure must satisfy RetryableError")
	assert.True(t, r.Retryable())
}

func TestRetryable_RequireFailure(t *testing.T) {
	t.Parallel()

	err := error(&RequireFailure{Expr: "f exists"})
	r, ok := retryableViaErrorsAs(err)
	require.True(t, ok, "RequireFailure must satisfy RetryableError")
	assert.True(t, r.Retryable())
}

func TestRetryable_RequireFailure_UnwrapsToErrRequireFailed(t *testing.T) {
	t.Parallel()

	// Existing call sites use errors.Is(err, ErrRequireFailed)
	// to halt at the script-loop boundary. RequireFailure
	// chains the sentinel via Unwrap so those checks keep
	// working after the typed-error layer landed.
	err := error(&RequireFailure{Expr: "boom"})
	assert.True(t, errors.Is(err, ErrRequireFailed))
}

func TestRetryable_SurvivesSyntaxErrorWrapping(t *testing.T) {
	t.Parallel()

	// frameAtSpan wraps evaluator errors in *SyntaxError so the
	// renderer can frame them. errors.As must still locate the
	// typed retryable underneath -- otherwise eventually would
	// silently classify a retryable failure as fatal.
	inner := &AssertFailure{Expr: "x == 1"}
	wrapped := frameAtSpan(Span{Pos: Pos{Line: 5, Col: 3}}, inner)
	r, ok := retryableViaErrorsAs(wrapped)
	require.True(t, ok)
	assert.True(t, r.Retryable())
}

func TestRetryable_FatalErrors_DoNotMatch(t *testing.T) {
	t.Parallel()

	// Untyped errors -- parse errors, unknown commands,
	// unbound variables, type errors -- propagate as plain
	// `error` values and must not satisfy RetryableError. An
	// eventually loop that retried these would delay
	// diagnosis without changing the outcome.
	tests := []error{
		errors.New("undefined variable: x"),
		errors.New("unknown command: foo"),
		errors.New("parse: unexpected token"),
		fmt.Errorf("wrapped: %w", errors.New("type error")),
	}
	for _, err := range tests {
		_, ok := retryableViaErrorsAs(err)
		assert.False(t, ok, "fatal error must not satisfy RetryableError: %v", err)
	}
}
