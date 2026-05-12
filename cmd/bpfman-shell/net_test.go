package main

import (
	"context"
	"encoding/json"
	"os"
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

func TestParseVethPairFlags_ExplicitMode_BothSpellings(t *testing.T) {
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
		assert.Falsef(t, f.AutoAddrs, "explicit-address args should not flip AutoAddrs")
		assert.Falsef(t, f.AutoNames, "explicit-name args should not flip AutoNames")
	}
}

func TestParseVethPairFlags_AutoAddrs_NoAddrFlags(t *testing.T) {
	t.Parallel()
	args := []string{"--ns=ns0", "--host-link=h0", "--peer-link=p0"}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	f, err := parseVethPairFlags(wargs)
	require.NoError(t, err)
	assert.True(t, f.AutoAddrs, "no addr flags should select auto-addresses")
	assert.False(t, f.AutoNames, "explicit names should not flip AutoNames")
	assert.Empty(t, f.HostAddrCIDR)
	assert.Empty(t, f.PeerAddrCIDR)
	assert.Empty(t, f.HostAddr)
	assert.Empty(t, f.PeerAddr)
}

func TestParseVethPairFlags_AutoNames_NoIdentityFlags(t *testing.T) {
	t.Parallel()
	args := []string{"--host-addr=198.51.100.1/30", "--peer-addr=198.51.100.2/30"}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	f, err := parseVethPairFlags(wargs)
	require.NoError(t, err)
	assert.True(t, f.AutoNames, "no identity flags should select auto-naming")
	assert.False(t, f.AutoAddrs, "explicit addresses should not flip AutoAddrs")
	assert.Empty(t, f.Ns)
	assert.Empty(t, f.HostLink)
	assert.Empty(t, f.PeerLink)
}

func TestParseVethPairFlags_AutoEverything_NoFlags(t *testing.T) {
	t.Parallel()
	f, err := parseVethPairFlags(nil)
	require.NoError(t, err, "an empty arg list should select auto-naming and auto-addresses")
	assert.True(t, f.AutoNames)
	assert.True(t, f.AutoAddrs)
	assert.Empty(t, f.Ns)
	assert.Empty(t, f.HostLink)
	assert.Empty(t, f.PeerLink)
	assert.Empty(t, f.HostAddrCIDR)
	assert.Empty(t, f.PeerAddrCIDR)
}

func TestParseVethPairFlags_PartialIdentityGroupIsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"only_ns", []string{"--ns=ns0"}, "--ns"},
		{"only_host_link", []string{"--host-link=h0"}, "--host-link"},
		{"only_peer_link", []string{"--peer-link=p0"}, "--peer-link"},
		{"ns_and_host_link", []string{"--ns=ns0", "--host-link=h0"}, "--peer-link"},
		{"ns_and_peer_link", []string{"--ns=ns0", "--peer-link=p0"}, "--host-link"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wargs := make([]shell.Arg, len(c.args))
			for i, a := range c.args {
				wargs[i] = shell.WordArg{Text: a}
			}
			_, err := parseVethPairFlags(wargs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be passed together or all omitted")
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

func TestParseVethPairFlags_HostAddrAloneIsError(t *testing.T) {
	t.Parallel()
	args := []string{"--ns=ns0", "--host-link=h0", "--peer-link=p0", "--host-addr=198.51.100.1/30"}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--host-addr was given without --peer-addr")
}

func TestParseVethPairFlags_PeerAddrAloneIsError(t *testing.T) {
	t.Parallel()
	args := []string{"--ns=ns0", "--host-link=h0", "--peer-link=p0", "--peer-addr=198.51.100.2/30"}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--peer-addr was given without --host-addr")
}

func TestParseVethPairFlags_NoRoutesFlagIsUnknown(t *testing.T) {
	t.Parallel()
	args := []string{
		"--ns=ns0", "--host-link=h0", "--peer-link=p0", "--no-routes",
	}
	wargs := make([]shell.Arg, len(args))
	for i, a := range args {
		wargs[i] = shell.WordArg{Text: a}
	}
	_, err := parseVethPairFlags(wargs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown flag "--no-routes"`)
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

// TestHandleNetRelease_AutoModeReleasesLease guards the
// handler-to-pool wiring on the release path. A NetPair that
// carries a Lease must trigger ReleasePoolSlot during teardown
// so the lockfile body picks up a released_at and the flock is
// dropped. The ip(8) commands inside handleNetRelease fail
// silently against the synthetic ns / link names; that is the
// existing behaviour and not what this test exercises.
func TestHandleNetRelease_AutoModeReleasesLease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lease, err := shell.AcquirePoolSlot(context.Background(), shell.PoolAcquireRequest{
		Root:        root,
		Origin:      "test_release.bpfman:1",
		NsName:      "ns-rel",
		LinkAName:   "vea-rel",
		LinkExists:  func(string) bool { return false },
		NetnsExists: func(string) bool { return false },
	})
	require.NoError(t, err)
	pair := &shell.NetPair{
		Ns:       "ns-rel",
		HostLink: "vea-rel",
		PeerLink: "veb-rel",
		HostAddr: lease.HostAddr,
		PeerAddr: lease.PeerAddr,
		Lease:    lease,
	}
	v, err := handleNet(builtinCtx{
		Ctx:  context.Background(),
		Cmd:  "net",
		Args: []shell.Arg{shell.WordArg{Text: "release"}, pairArg(pair)},
	})
	require.NoError(t, err)
	env, ok := v.Origin().(shell.Envelope)
	require.True(t, ok)
	assert.True(t, env.OK)

	body, err := os.ReadFile(slotLockPathForTest(t, root, lease.Slot))
	require.NoError(t, err)
	var prov struct {
		Origin     string `json:"origin"`
		NsName     string `json:"ns_name"`
		LinkAName  string `json:"link_a_name"`
		ReleasedAt string `json:"released_at"`
	}
	require.NoError(t, json.Unmarshal(body, &prov))
	assert.Equal(t, "test_release.bpfman:1", prov.Origin)
	assert.Equal(t, "ns-rel", prov.NsName)
	assert.Equal(t, "vea-rel", prov.LinkAName)
	assert.NotEmpty(t, prov.ReleasedAt, "release path must write released_at into the slot body")
	assert.True(t, pair.IsReleased())
}

// slotLockPathForTest reproduces the pool's slot-to-path mapping
// for the integration test above. Kept here rather than exported
// from shell/netpool.go because it is a one-line implementation
// detail, not a stable API contract.
func slotLockPathForTest(t *testing.T, root string, slot uint32) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	want := []byte{byte('0' + slot/10), byte('0' + slot%10)}
	for _, e := range entries {
		name := e.Name()
		if len(name) >= 2 && name[0] == want[0] && name[1] == want[1] {
			return root + "/" + name
		}
	}
	t.Fatalf("no lockfile for slot %d under %s", slot, root)
	return ""
}
