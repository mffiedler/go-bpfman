package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValueFromEnvelope_OriginAndKind(t *testing.T) {
	t.Parallel()

	e := Envelope{OK: true, Code: 0, Stdout: "hello", Stderr: ""}
	v := ValueFromEnvelope(e)

	assert.Equal(t, OriginEnvelope, v.Kind())
	got, ok := v.Origin().(Envelope)
	require.True(t, ok, "Origin() should be Envelope, got %T", v.Origin())
	assert.Equal(t, e, got)
}

func TestValueFromEnvelope_FieldAccess(t *testing.T) {
	t.Parallel()

	e := Envelope{
		OK:     false,
		Code:   2,
		Stdout: "out",
		Stderr: "boom",
	}
	v := ValueFromEnvelope(e)

	cases := []struct {
		path string
		want string
	}{
		{"ok", "false"},
		{"code", "2"},
		{"stdout", "out"},
		{"stderr", "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := v.Lookup("$r", tc.path)
			require.NoError(t, err, "lookup %q", tc.path)
			s, err := got.Scalar()
			require.NoError(t, err)
			assert.Equal(t, tc.want, s)
		})
	}
}

func TestValueFromEnvelope_PIDOmittedWhenSync(t *testing.T) {
	t.Parallel()

	v := ValueFromEnvelope(Envelope{OK: true, Code: 0})
	_, err := v.LookupValue("$r", "pid")
	require.Error(t, err, "sync envelope must omit pid")
	assert.Contains(t, err.Error(), "field pid not found")
}

func TestValueFromEnvelope_PIDPresentWhenAsync(t *testing.T) {
	t.Parallel()

	v := ValueFromEnvelope(Envelope{OK: true, Code: 0, HasPID: true, PID: 4321})
	got, err := v.Lookup("$r", "pid")
	require.NoError(t, err)
	s, err := got.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "4321", s)
}

func TestValueFromEnvelope_ValuePayloadWalks(t *testing.T) {
	t.Parallel()

	// The inner payload is a Value carrying a JSON-tree map. The
	// envelope's path-walker reaches into it through the "value"
	// key, so $r.value.id resolves the same way an inline map
	// lookup would.
	inner := ValueFromMap(map[string]any{"id": "42", "name": "p"})
	v := ValueFromEnvelope(Envelope{OK: true, Code: 0, Value: inner})

	id, err := v.Lookup("$r", "value.id")
	require.NoError(t, err)
	idStr, err := id.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", idStr)

	name, err := v.Lookup("$r", "value.name")
	require.NoError(t, err)
	nameStr, err := name.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "p", nameStr)
}

func TestValueFromEnvelope_ValueAbsentLooksLikeNull(t *testing.T) {
	t.Parallel()

	// External commands and the explicit `exec` escape hatch
	// leave Value zero. The envelope still exposes "value" as a
	// reachable key so $r.value lookups do not error; the path
	// returns a null-shaped Value.
	v := ValueFromEnvelope(Envelope{OK: true, Code: 0})

	got, err := v.LookupValue("$r", "value")
	require.NoError(t, err)
	assert.True(t, got.IsNil(), "absent payload should look like a nil/null lookup result")
}

func TestValueFromEnvelope_OriginPreservesTypedPayload(t *testing.T) {
	t.Parallel()

	// The Envelope struct recovered via Origin() must carry the
	// inner Value with its own origin tag intact. Path-lookup
	// through the JSON-tree mirror loses that tag (by design,
	// since the mirror is a map[string]any), so Origin() is the
	// route consumers use to reach the typed Go payload.
	type sample struct {
		ID string `json:"id"`
	}
	inner, err := ValueFromStruct(sample{ID: "abc"})
	require.NoError(t, err)
	inner = inner.WithKind(OriginProgram)

	v := ValueFromEnvelope(Envelope{OK: true, Code: 0, Value: inner})

	env, ok := v.Origin().(Envelope)
	require.True(t, ok)
	assert.Equal(t, OriginProgram, env.Value.Kind())
	got, ok := env.Value.Origin().(sample)
	require.True(t, ok, "inner payload Origin should be sample, got %T", env.Value.Origin())
	assert.Equal(t, "abc", got.ID)
}

func TestOriginKind_EnvelopeString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "envelope", OriginEnvelope.String())
}
