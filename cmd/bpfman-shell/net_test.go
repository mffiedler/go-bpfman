package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/shell"
)

// callNet invokes handleNet with the given word-arg sequence and
// returns the (value, error) result. Tests use this for failures
// that surface before any ip(8) invocation; the happy path
// requires root and lives in the e2e corpus.
func callNet(args ...string) (shell.Value, error) {
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	return handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: wargs,
	})
}

// pairArg wraps a *shell.NetPair as a StructuredValueArg the way
// the argument expander would produce when a script writes
// $pair. Tests that need to dispatch handleNet on a $pair pass
// the result here as args[0].
func pairArg(p *shell.NetPair) shell.Arg {
	return shell.StructuredValueArg{
		Name:  "pair",
		Value: shell.ValueFromNetPair(p),
	}
}

// scalarPairArg constructs a non-NetPair structured arg so tests
// can confirm the kind check on $pair-receiving subcommands fires
// for the wrong shape.
func scalarPairArg() shell.Arg {
	v := shell.StringValue("not a pair")
	return shell.StructuredValueArg{Name: "x", Value: v}
}

func TestHandleNet_NoSubcommand(t *testing.T) {
	t.Parallel()
	_, err := callNet()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subcommand required")
}

func TestHandleNet_UnknownSubcommand(t *testing.T) {
	t.Parallel()
	_, err := callNet("bridge")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown subcommand "bridge"`)
	assert.Contains(t, err.Error(), "exec, release, start, veth-pair")
}

func TestParseVethPairFlags_BothSpellings(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{
			"--ns=ns0", "--host-link=h0", "--host-addr=198.51.100.1/32",
			"--peer-link=p0", "--peer-addr=198.51.100.2/32",
		},
		{
			"--ns", "ns0", "--host-link", "h0", "--host-addr", "198.51.100.1/32",
			"--peer-link", "p0", "--peer-addr", "198.51.100.2/32",
		},
	}
	for _, args := range cases {
		wargs := make([]shell.Arg, len(args))
		for i, a := range args {
			wargs[i] = shell.WordArg{Text: a}
		}
		f, err := parseVethPairFlags(wargs)
		require.NoErrorf(t, err, "args=%v", args)
		assert.Equal(t, "ns0", f.Ns)
		assert.Equal(t, "h0", f.HostLink)
		assert.Equal(t, "p0", f.PeerLink)
		assert.Equal(t, "198.51.100.1/32", f.HostAddrCIDR)
		assert.Equal(t, "198.51.100.2/32", f.PeerAddrCIDR)
		assert.Equal(t, "198.51.100.1", f.HostAddr)
		assert.Equal(t, "198.51.100.2", f.PeerAddr)
		assert.False(t, f.NoRoutes)
	}
}

func TestParseVethPairFlags_NoRoutesAccepted(t *testing.T) {
	t.Parallel()
	args := []string{
		"--ns=ns0", "--host-link=h0", "--host-addr=198.51.100.1/32",
		"--peer-link=p0", "--peer-addr=198.51.100.2/32",
		"--no-routes",
	}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	f, err := parseVethPairFlags(wargs)
	require.NoError(t, err)
	assert.True(t, f.NoRoutes)
}

func TestParseVethPairFlags_MissingRequiredFlags(t *testing.T) {
	t.Parallel()
	wargs := []shell.Arg{
		shell.WordArg{Text: "--ns=ns0"},
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required flag(s)")
	assert.Contains(t, err.Error(), "--host-link")
	assert.Contains(t, err.Error(), "--host-addr")
	assert.Contains(t, err.Error(), "--peer-link")
	assert.Contains(t, err.Error(), "--peer-addr")
}

func TestParseVethPairFlags_BareAddressRejected(t *testing.T) {
	t.Parallel()
	args := []string{
		"--ns=ns0", "--host-link=h0", "--host-addr=198.51.100.1",
		"--peer-link=p0", "--peer-addr=198.51.100.2/32",
	}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--host-addr")
	assert.Contains(t, err.Error(), "CIDR form")
}

func TestParseVethPairFlags_IPv6Rejected(t *testing.T) {
	t.Parallel()
	args := []string{
		"--ns=ns0", "--host-link=h0", "--host-addr=2001:db8::1/128",
		"--peer-link=p0", "--peer-addr=198.51.100.2/32",
	}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IPv4")
}

func TestParseVethPairFlags_UnknownFlag(t *testing.T) {
	t.Parallel()
	args := []string{
		"--ns=ns0", "--host-link=h0", "--host-addr=198.51.100.1/32",
		"--peer-link=p0", "--peer-addr=198.51.100.2/32",
		"--bogus=x",
	}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown flag "--bogus=x"`)
}

func TestParseVethPairFlags_UnexpectedPositional(t *testing.T) {
	t.Parallel()
	args := []string{
		"--ns=ns0", "stray", "--host-link=h0", "--host-addr=198.51.100.1/32",
		"--peer-link=p0", "--peer-addr=198.51.100.2/32",
	}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unexpected positional argument "stray"`)
}

func TestParseVethPairFlags_TrailingFlagWithoutValue(t *testing.T) {
	t.Parallel()
	wargs := []shell.Arg{shell.WordArg{Text: "--ns"}}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ns requires a value")
}

func TestHandleNetRelease_NoArgs(t *testing.T) {
	t.Parallel()
	_, err := callNet("release")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one $pair argument")
}

func TestHandleNetRelease_NonPairArg(t *testing.T) {
	t.Parallel()
	_, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "release"}, scalarPairArg()},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$pair argument")
}

func TestHandleNetRelease_IdempotentOnAlreadyReleased(t *testing.T) {
	t.Parallel()
	pair := &shell.NetPair{
		Ns:       "ns0",
		HostLink: "h0",
		PeerLink: "p0",
		HostAddr: "198.51.100.1",
		PeerAddr: "198.51.100.2",
	}
	pair.MarkReleased()
	v, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "release"}, pairArg(pair)},
	})
	require.NoError(t, err)
	require.Equal(t, shell.OriginEnvelope, v.Kind())
	env, ok := v.Origin().(shell.Envelope)
	require.True(t, ok, "release should publish a shell.Envelope as origin")
	assert.True(t, env.OK)
	assert.Equal(t, 0, env.Code)
}

func TestHandleNetExec_TooFewArgs(t *testing.T) {
	t.Parallel()
	_, err := callNet("exec")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$pair and a command")
}

func TestHandleNetExec_NonPairArg(t *testing.T) {
	t.Parallel()
	_, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "exec"}, scalarPairArg(), shell.WordArg{Text: "true"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$pair argument")
}

func TestHandleNetExec_RejectsReleasedHandle(t *testing.T) {
	t.Parallel()
	pair := &shell.NetPair{Ns: "ns0", HostLink: "h0", PeerLink: "p0", HostAddr: "1.2.3.4", PeerAddr: "1.2.3.5"}
	pair.MarkReleased()
	_, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "exec"}, pairArg(pair), shell.WordArg{Text: "true"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "released")
}

func TestHandleNetStart_TooFewArgs(t *testing.T) {
	t.Parallel()
	_, err := callNet("start")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$pair and a command")
}

func TestHandleNetStart_NonPairArg(t *testing.T) {
	t.Parallel()
	_, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "start"}, scalarPairArg(), shell.WordArg{Text: "sleep"}, shell.WordArg{Text: "0"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$pair argument")
}

func TestHandleNetStart_RejectsReleasedHandle(t *testing.T) {
	t.Parallel()
	pair := &shell.NetPair{Ns: "ns0", HostLink: "h0", PeerLink: "p0", HostAddr: "1.2.3.4", PeerAddr: "1.2.3.5"}
	pair.MarkReleased()
	_, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "start"}, pairArg(pair), shell.WordArg{Text: "sleep"}, shell.WordArg{Text: "0"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "released")
}

// TestNetPair_FieldsRemainReadableAfterRelease confirms the
// invariant the doc surface promises: net release marks the
// handle consumed, but $pair.host_link / $pair.peer_addr / ...
// still resolve through the standard path-walker so a
// downstream print or interpolation does not error.
func TestNetPair_FieldsRemainReadableAfterRelease(t *testing.T) {
	t.Parallel()
	pair := &shell.NetPair{
		Ns:       "ns0",
		HostLink: "h0",
		PeerLink: "p0",
		HostAddr: "198.51.100.1",
		PeerAddr: "198.51.100.2",
	}
	pair.MarkReleased()
	v := shell.ValueFromNetPair(pair)
	got, err := v.Lookup("$pair", "host_link")
	require.NoError(t, err)
	s, err := got.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "h0", s)
}

// TestNetBuiltin_RegisteredInRegistry confirms the registry
// entry is reachable so 'help net' and dispatcher lookup both
// see it. Missing entry would cause a regression where 'net' falls
// through to the external-command runner and silently spawns
// a nonexistent /usr/bin/net binary.
func TestNetBuiltin_RegisteredInRegistry(t *testing.T) {
	t.Parallel()
	entry, ok := builtinRegistry["net"]
	require.Truef(t, ok, "net is not in builtinRegistry")
	assert.NotNil(t, entry.Handler)
	assert.NotEmpty(t, entry.Usage)
	assert.NotEmpty(t, entry.Summary)
}
