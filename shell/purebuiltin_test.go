package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPureBuiltinRegistry_JqRegisteredByPackageInit(t *testing.T) {
	t.Parallel()

	pb, ok := LookupPureBuiltin("jq")
	require.True(t, ok, "shell package init should register jq")
	assert.Equal(t, "jq", pb.Name)
	assert.Equal(t, 2, pb.Arity)
	assert.False(t, pb.ReturnShape.Sealed, "jq's return shape is unknown so any path access is permitted")
	assert.Equal(t, OriginUnknown, pb.ReturnShape.Kind)
}

func TestPureBuiltinRegistry_UnregisteredNameLookupFails(t *testing.T) {
	t.Parallel()

	_, ok := LookupPureBuiltin("definitely-not-a-pure-builtin")
	assert.False(t, ok)
}

func TestPureBuiltinRegistry_RegisterOverwrites(t *testing.T) {
	t.Parallel()

	// Use a name unlikely to collide with anything real so the
	// test stays isolated from concurrent registrations.
	const name = "__purebuiltin_test_overwrite__"
	RegisterPureBuiltin(name, 1, KindShape(OriginScalar))
	t.Cleanup(func() { delete(pureBuiltinRegistry, name) })

	pb, ok := LookupPureBuiltin(name)
	require.True(t, ok)
	assert.Equal(t, 1, pb.Arity)
	assert.Equal(t, OriginScalar, pb.ReturnShape.Kind)

	RegisterPureBuiltin(name, 3, KindShape(OriginBool))
	pb, ok = LookupPureBuiltin(name)
	require.True(t, ok)
	assert.Equal(t, 3, pb.Arity)
	assert.Equal(t, OriginBool, pb.ReturnShape.Kind)
}
