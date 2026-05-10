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

func TestValueFromEnvelope_ValueFieldNotPresent(t *testing.T) {
	t.Parallel()

	// The result envelope carries execution metadata only. There
	// is no "value" key on the envelope itself; the provider's
	// primary lives in its own bind slot.
	v := ValueFromEnvelope(Envelope{OK: true, Code: 0})
	_, err := v.LookupValue("$r", "value")
	require.Error(t, err, "envelope must not expose a 'value' field")
	assert.Contains(t, err.Error(), "field value not found")
}

func TestOriginKind_EnvelopeString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "result", OriginEnvelope.String())
}
