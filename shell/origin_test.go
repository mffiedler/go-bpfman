package shell

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOriginKind_String(t *testing.T) {
	cases := []struct {
		kind OriginKind
		want string
	}{
		{OriginUnknown, "unknown"},
		{OriginScalar, "scalar"},
		{OriginBool, "boolean"},
		{OriginProgram, "program"},
		{OriginLink, "link"},
		{OriginDispatcher, "dispatcher"},
		{OriginMap, "map"},
		{OriginExecResult, "exec.result"},
		{OriginKind(999), "OriginKind(999)"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.kind.String())
		})
	}
}

func TestValue_KindDefaultsToUnknown(t *testing.T) {
	v, err := ValueFromJSON([]byte(`{"a":1}`))
	require.NoError(t, err)
	assert.Equal(t, OriginUnknown, v.Kind())

	v = ValueFromMap(map[string]any{"a": 1})
	assert.Equal(t, OriginUnknown, v.Kind())

	type S struct{ X int }
	v, err = ValueFromStruct(S{X: 1})
	require.NoError(t, err)
	assert.Equal(t, OriginUnknown, v.Kind())
}

func TestValue_ConstructorKinds(t *testing.T) {
	assert.Equal(t, OriginScalar, StringValue("x").Kind())
	assert.Equal(t, OriginBool, BoolValue(true).Kind())
}

func TestValue_WithKind(t *testing.T) {
	v := StringValue("x").WithKind(OriginProgram)
	assert.Equal(t, OriginProgram, v.Kind())

	// WithKind returns a copy; original is unchanged.
	base := StringValue("x")
	tagged := base.WithKind(OriginLink)
	assert.Equal(t, OriginScalar, base.Kind())
	assert.Equal(t, OriginLink, tagged.Kind())
}

func TestExpectOrigin_Matches(t *testing.T) {
	v := StringValue("x").WithKind(OriginProgram)
	assert.NoError(t, ExpectOrigin(v, "$prog", OriginProgram))
	assert.NoError(t, ExpectOrigin(v, "$prog", OriginProgram, OriginLink))
}

func TestExpectOrigin_UnknownIsWildcard(t *testing.T) {
	v, err := ValueFromJSON([]byte(`{"record":{"id":1}}`))
	require.NoError(t, err)
	assert.NoError(t, ExpectOrigin(v, "$x", OriginProgram))
	assert.NoError(t, ExpectOrigin(v, "$x", OriginLink))
}

func TestExpectOrigin_Mismatch(t *testing.T) {
	v := StringValue("x").WithKind(OriginProgram)
	err := ExpectOrigin(v, "$prog", OriginLink)
	require.Error(t, err)

	var mismatch *OriginMismatchError
	require.True(t, errors.As(err, &mismatch))
	assert.Equal(t, "$prog", mismatch.VarName)
	assert.Equal(t, OriginProgram, mismatch.Got)
	assert.Equal(t, []OriginKind{OriginLink}, mismatch.Want)
}

func TestOriginMismatchError_Message(t *testing.T) {
	cases := []struct {
		name string
		err  *OriginMismatchError
		want string
	}{
		{
			name: "single expected",
			err:  &OriginMismatchError{VarName: "$prog", Got: OriginProgram, Want: []OriginKind{OriginLink}},
			want: `variable "$prog" is a program; expected link`,
		},
		{
			name: "multiple expected",
			err:  &OriginMismatchError{VarName: "$x", Got: OriginScalar, Want: []OriginKind{OriginProgram, OriginLink}},
			want: `variable "$x" is a scalar; expected one of program, link`,
		},
		{
			name: "no varname",
			err:  &OriginMismatchError{Got: OriginLink, Want: []OriginKind{OriginProgram}},
			want: `value is a link; expected program`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}
