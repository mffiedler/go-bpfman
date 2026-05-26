package runtime

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
)

func TestOriginKind_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind semantics.OriginKind
		want string
	}{
		{semantics.OriginUnknown, "unknown"},
		{semantics.OriginScalar, "scalar"},
		{semantics.OriginBool, "boolean"},
		{semantics.OriginProgram, "program"},
		{semantics.OriginLink, "link"},
		{semantics.OriginDispatcher, "dispatcher"},
		{semantics.OriginMap, "map"},
		{semantics.OriginEnvelope, "result"},
		{semantics.OriginKind(999), "OriginKind(999)"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.kind.String())
		})
	}
}

func TestValue_KindDefaultsToUnknown(t *testing.T) {
	t.Parallel()

	v, err := ValueFromJSON([]byte(`{"a":1}`))
	require.NoError(t, err)
	assert.Equal(t, semantics.OriginUnknown, v.Kind())

	v = ValueFromMap(map[string]any{"a": 1})
	assert.Equal(t, semantics.OriginUnknown, v.Kind())

	type S struct{ X int }
	v, err = ValueFromStruct(S{X: 1})
	require.NoError(t, err)
	assert.Equal(t, semantics.OriginUnknown, v.Kind())
}

func TestValue_ConstructorKinds(t *testing.T) {
	t.Parallel()

	assert.Equal(t, semantics.OriginScalar, StringValue("x").Kind())
	assert.Equal(t, semantics.OriginBool, BoolValue(true).Kind())
}

func TestValue_WithKind(t *testing.T) {
	t.Parallel()

	v := StringValue("x").WithKind(semantics.OriginProgram)
	assert.Equal(t, semantics.OriginProgram, v.Kind())

	// WithKind returns a copy; original is unchanged.
	base := StringValue("x")
	tagged := base.WithKind(semantics.OriginLink)
	assert.Equal(t, semantics.OriginScalar, base.Kind())
	assert.Equal(t, semantics.OriginLink, tagged.Kind())
}

func TestExpectOrigin_Matches(t *testing.T) {
	t.Parallel()

	v := StringValue("x").WithKind(semantics.OriginProgram)
	assert.NoError(t, ExpectOrigin(v, "$prog", semantics.OriginProgram))
	assert.NoError(t, ExpectOrigin(v, "$prog", semantics.OriginProgram, semantics.OriginLink))
}

func TestExpectOrigin_UnknownIsWildcard(t *testing.T) {
	t.Parallel()

	v, err := ValueFromJSON([]byte(`{"record":{"id":1}}`))
	require.NoError(t, err)
	assert.NoError(t, ExpectOrigin(v, "$x", semantics.OriginProgram))
	assert.NoError(t, ExpectOrigin(v, "$x", semantics.OriginLink))
}

func TestExpectOrigin_Mismatch(t *testing.T) {
	t.Parallel()

	v := StringValue("x").WithKind(semantics.OriginProgram)
	err := ExpectOrigin(v, "$prog", semantics.OriginLink)
	require.Error(t, err)

	var mismatch *OriginMismatchError
	require.True(t, errors.As(err, &mismatch))
	assert.Equal(t, "$prog", mismatch.VarName)
	assert.Equal(t, semantics.OriginProgram, mismatch.Got)
	assert.Equal(t, []semantics.OriginKind{semantics.OriginLink}, mismatch.Want)
}

func TestOriginMismatchError_Message(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *OriginMismatchError
		want string
	}{
		{
			name: "single expected",
			err:  &OriginMismatchError{VarName: "$prog", Got: semantics.OriginProgram, Want: []semantics.OriginKind{semantics.OriginLink}},
			want: `variable "$prog" is a program; expected link`,
		},
		{
			name: "multiple expected",
			err:  &OriginMismatchError{VarName: "$x", Got: semantics.OriginScalar, Want: []semantics.OriginKind{semantics.OriginProgram, semantics.OriginLink}},
			want: `variable "$x" is a scalar; expected one of program, link`,
		},
		{
			name: "no varname",
			err:  &OriginMismatchError{Got: semantics.OriginLink, Want: []semantics.OriginKind{semantics.OriginProgram}},
			want: `value is a link; expected program`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}
