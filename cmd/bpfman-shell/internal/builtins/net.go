// net is the e2e built-in for paired-veth single-netns
// topologies used by TC / TCX / XDP dispatcher tests. The
// script chooses the topology parameters (names, addresses);
// the built-in owns the imperative order of ip(8) calls, the
// idempotent pre-clean against leftover kernel state, and the
// LIFO-correct teardown. See docs/PLAN-net-builtin.md for the
// design rationale.
//
// net is for paired-veth single-netns topologies used by
// TC / TCX / XDP dispatcher tests. A richer network-fixture
// surface, if needed, lives in its own subsystem, not under
// the net builtin.
package builtins

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
	"github.com/frobware/go-bpfman/ns/netns"
)

func init() {
	Register(driver.Builtin{
		Name:     "net",
		Handler:  handleNet,
		Category: driver.CategoryJobs,
		Usage: "net veth-pair --ns=NS --host-link=NAME --host-addr=CIDR --peer-link=NAME --peer-addr=CIDR [--no-routes]  |  " +
			"net release $pair  |  net exec $pair CMD ARGS...  |  net start $pair CMD ARGS...",
		Summary: "Paired-veth single-netns topology fixture for TC / TCX / XDP dispatcher tests.",
		Detail: "net is the e2e built-in for the topology dispatcher tests share: a single " +
			"veth pair, a netns the peer end lives in, two /32 addresses, and the two " +
			"symmetric routes that make the pair pingable. veth-pair builds the whole " +
			"thing in one call and returns a $pair handle whose fields (ns, host_link, " +
			"peer_link, host_addr, peer_addr) thread through 'bpfman link attach -i " +
			"$pair.host_link' and 'net exec $pair ping $pair.peer_addr'. release tears " +
			"the topology down in LIFO order and is idempotent (re-release is a no-op; " +
			"missing resources are fine). exec runs a command in the netns and captures " +
			"the result envelope (sync); start launches a command in the netns as a " +
			"background $job (async). Operational use after release is a runtime error; " +
			"field reads stay valid. Raw ip(8) remains the documented escape hatch for " +
			"topologies net does not cover (bridges, VLANs, IPv6, multiple pairs).",
	})
}

// handleNet is the registry handler for `net <subcommand> ...`.
// Subcommand dispatch is a small in-handler switch (parallel to
// the bpfman first-noun dispatch) rather than per-subcommand
// files; the operations are small enough not to warrant the
// extra fan-out.
func handleNet(c driver.Ctx) (runtime.Value, error) {
	if len(c.Args) == 0 {
		return runtime.Value{}, fmt.Errorf("net: subcommand required (valid: exec, release, start, veth-pair)")
	}
	sub := driver.ArgText(c.Args[0])
	rest := c.Args[1:]
	switch sub {
	case "veth-pair":
		return handleNetVethPair(c.Ctx, c.Pos.Cite(), rest)
	case "release":
		return handleNetRelease(c.Ctx, rest)
	case "exec":
		env, err := NetExecEnvelope(c.Ctx, rest)
		if err != nil {
			return runtime.Value{}, err
		}
		return runtime.ValueFromEnvelope(env), nil
	case "start":
		return handleNetStart(c.Ctx, c.Env, c.Pos.Cite(), rest)
	default:
		return runtime.Value{}, fmt.Errorf("net: unknown subcommand %q (valid: exec, release, start, veth-pair)", sub)
	}
}

// netPairFromArg unwraps a $pair argument into the underlying
// *runtime.NetPair without checking the lifecycle latch. Used by
// net release where re-release is idempotent and the latch is
// re-checked inside the release handler.
func netPairFromArg(a runtime.Arg) (*runtime.NetPair, error) {
	sva, ok := a.(runtime.StructuredValueArg)
	if !ok {
		return nil, fmt.Errorf("expected a $pair argument, got %T", a)
	}
	if sva.Value.Kind() != semantics.OriginNetPair {
		return nil, fmt.Errorf("expected a $pair argument, got a %s value", sva.Value.Kind())
	}
	pair, ok := sva.Value.Origin().(*runtime.NetPair)
	if !ok {
		return nil, fmt.Errorf("$pair has no underlying handle (got %T)", sva.Value.Origin())
	}
	return pair, nil
}

// ensureNetPair unwraps a $pair argument and rejects a released
// handle. Used by net exec and net start, where operational use
// of a consumed topology is a runtime error. net release reaches
// for netPairFromArg directly because the second release is a
// harmless idempotent no-op.
func ensureNetPair(a runtime.Arg) (*runtime.NetPair, error) {
	pair, err := netPairFromArg(a)
	if err != nil {
		return nil, err
	}
	if pair.IsReleased() {
		return nil, fmt.Errorf("$pair has been released; operational use of the handle is invalid after release")
	}
	return pair, nil
}

// vethPairFlags is the parsed form of `net veth-pair`'s flag set.
// HostAddrCIDR / PeerAddrCIDR are the full prefix forms the
// caller passed via --host-addr / --peer-addr; HostAddr /
// PeerAddr are the matching bare addresses. Both pairs are empty
// in auto-address mode (AutoAddrs == true), where the address
// pool fills them in at acquire time. Ns / HostLink / PeerLink
// are empty in auto-naming mode (AutoNames == true), where
// generateTopologyNames fills them in at handler entry.
type vethPairFlags struct {
	Ns           string
	HostLink     string
	HostAddrCIDR string
	HostAddr     string
	PeerLink     string
	PeerAddrCIDR string
	PeerAddr     string

	// AutoAddrs is true when neither --host-addr nor --peer-addr
	// was passed; the pool allocates a /30 in that case. Passing
	// exactly one of --host-addr / --peer-addr is a parse error.
	AutoAddrs bool

	// AutoNames is true when none of --ns / --host-link /
	// --peer-link were passed; the handler then derives all three
	// from generateTopologyNames so two concurrent runs of
	// the same script do not collide on identity. Passing any
	// proper subset is a parse error: the three identity flags
	// move together.
	AutoNames bool
}

// parseVethPairFlags walks `net veth-pair`'s arg list, accepting
// both the `--name=value` and `--name value` spellings for each
// flag (matching fire's convention).
//
// All five flags are optional but split into two mutually-
// required groups so partial specifications are caught early:
//
//   - Identity group: --ns, --host-link, --peer-link. Pass all
//     three for explicit naming, none for pool-derived auto
//     naming (the handler then calls
//     generateTopologyNames).
//   - Address group: --host-addr, --peer-addr. Pass both for
//     explicit addressing, neither for pool-allocated /30.
//
// The groups are independent: explicit addresses with auto names
// (or vice versa) are both valid. Unknown flags and positional
// arguments are rejected with the offending token quoted so the
// user can correct it.
func parseVethPairFlags(args []runtime.Arg) (vethPairFlags, error) {
	var f vethPairFlags
	for i := 0; i < len(args); {
		text := driver.ArgText(args[i])
		switch {
		case text == "--ns":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--ns requires a value")
			}
			f.Ns = driver.ArgText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--ns="):
			f.Ns = strings.TrimPrefix(text, "--ns=")
			i++
		case text == "--host-link":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--host-link requires a value")
			}
			f.HostLink = driver.ArgText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--host-link="):
			f.HostLink = strings.TrimPrefix(text, "--host-link=")
			i++
		case text == "--host-addr":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--host-addr requires a value")
			}
			f.HostAddrCIDR = driver.ArgText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--host-addr="):
			f.HostAddrCIDR = strings.TrimPrefix(text, "--host-addr=")
			i++
		case text == "--peer-link":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--peer-link requires a value")
			}
			f.PeerLink = driver.ArgText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--peer-link="):
			f.PeerLink = strings.TrimPrefix(text, "--peer-link=")
			i++
		case text == "--peer-addr":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--peer-addr requires a value")
			}
			f.PeerAddrCIDR = driver.ArgText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--peer-addr="):
			f.PeerAddrCIDR = strings.TrimPrefix(text, "--peer-addr=")
			i++
		case strings.HasPrefix(text, "--"):
			return f, fmt.Errorf("unknown flag %q", text)
		default:
			return f, fmt.Errorf("unexpected positional argument %q (net veth-pair takes only flags)", text)
		}
	}

	// Identity group: --ns / --host-link / --peer-link must move
	// together. Count populated flags; 0 = auto-naming, 3 =
	// explicit, anything else is a partial spec and rejected.
	nameCount := 0
	if f.Ns != "" {
		nameCount++
	}
	if f.HostLink != "" {
		nameCount++
	}
	if f.PeerLink != "" {
		nameCount++
	}
	switch nameCount {
	case 0:
		f.AutoNames = true
	case 3:
		// explicit naming, nothing to fill in
	default:
		given, missing := identityFlagsBreakdown(f)
		return f, fmt.Errorf("--ns / --host-link / --peer-link must be passed together or all omitted (got %s; missing %s)", given, missing)
	}

	// Address group: same shape, two flags.
	switch {
	case f.HostAddrCIDR == "" && f.PeerAddrCIDR == "":
		f.AutoAddrs = true
	case f.HostAddrCIDR != "" && f.PeerAddrCIDR != "":
		bare, err := parseIPv4Prefix(f.HostAddrCIDR)
		if err != nil {
			return f, fmt.Errorf("--host-addr: %w", err)
		}
		f.HostAddr = bare
		bare, err = parseIPv4Prefix(f.PeerAddrCIDR)
		if err != nil {
			return f, fmt.Errorf("--peer-addr: %w", err)
		}
		f.PeerAddr = bare
	case f.HostAddrCIDR == "":
		return f, fmt.Errorf("--peer-addr was given without --host-addr (pass both for explicit-address mode or neither for auto mode)")
	default:
		return f, fmt.Errorf("--host-addr was given without --peer-addr (pass both for explicit-address mode or neither for auto mode)")
	}
	return f, nil
}

// identityFlagsBreakdown reports which identity flags were given
// and which were missing, formatted for the parse-error message
// when the group is partially specified. Reading the message
// makes the fix obvious without forcing the user to re-scan their
// argv.
func identityFlagsBreakdown(f vethPairFlags) (given, missing string) {
	pairs := []struct {
		name, val string
	}{
		{"--ns", f.Ns},
		{"--host-link", f.HostLink},
		{"--peer-link", f.PeerLink},
	}
	var givenNames, missingNames []string
	for _, p := range pairs {
		if p.val != "" {
			givenNames = append(givenNames, p.name)
		} else {
			missingNames = append(missingNames, p.name)
		}
	}
	return strings.Join(givenNames, ", "), strings.Join(missingNames, ", ")
}

// parseIPv4Prefix validates s as an IPv4 prefix in CIDR form and
// returns the bare address. v1 is IPv4-only; the IPv6 case is the
// future-expansion notch the plan calls out in section 9 and
// rejects loudly here so a stray AAAA address never silently
// produces a malformed `ip addr add` invocation.
func parseIPv4Prefix(s string) (string, error) {
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		return "", fmt.Errorf("%q is not in CIDR form (e.g. 198.51.100.1/32): %w", s, err)
	}
	if !prefix.Addr().Is4() {
		return "", fmt.Errorf("%q is not an IPv4 address (net is IPv4-only in v1)", s)
	}
	return prefix.Addr().String(), nil
}

// handleNetVethPair builds the paired-veth single-netns topology
// in the canonical order: best-effort pre-clean, (auto mode
// only) acquire a pool slot, create the netns, create the veth
// pair, move the peer end into the netns, bring both ends up,
// and assign addresses. The /30 layout means the kernel
// installs the connected route automatically; this builtin does
// not manage explicit routes in either auto or explicit mode.
// A mid-setup failure rolls the partially-built state back --
// including releasing the pool slot -- so a leaked script does
// not require manual `ip netns del` to recover.
func handleNetVethPair(ctx context.Context, origin string, args []runtime.Arg) (runtime.Value, error) {
	f, err := parseVethPairFlags(args)
	if err != nil {
		return runtime.Value{}, fmt.Errorf("net veth-pair: %w", err)
	}

	// Auto-naming runs before pool acquisition so the slot's
	// provenance body records the names the next acquirer's leak
	// check will compare against. The pool's flock + released_at
	// is what protects ordering; the identity is just the label.
	if f.AutoNames {
		f.Ns, f.HostLink, f.PeerLink = generateTopologyNames()
	}

	var lease *poolLease
	hostCIDR := f.HostAddrCIDR
	peerCIDR := f.PeerAddrCIDR
	hostAddr := f.HostAddr
	peerAddr := f.PeerAddr
	if f.AutoAddrs {
		lease, err = acquirePoolSlot(poolAcquireRequest{
			origin:    origin,
			nsName:    f.Ns,
			linkAName: f.HostLink,
		})
		if err != nil {
			return runtime.Value{}, fmt.Errorf("net veth-pair: %w", err)
		}
		hostCIDR = lease.hostCIDR
		peerCIDR = lease.peerCIDR
		hostAddr = lease.hostAddr
		peerAddr = lease.peerAddr
	}

	// Best-effort pre-clean against leftover state from a prior
	// failed run. A residual veth alone is enough to fail the
	// link-add later; a residual netns alone is enough to fail
	// the netns-add. Both deletes are silent on missing-resource.
	runIPIgnoreErr(ctx, "link", "del", f.HostLink)
	runIPIgnoreErr(ctx, "netns", "del", f.Ns)

	var nsCreated, linkCreated bool
	rollback := func() {
		if linkCreated {
			runIPIgnoreErr(ctx, "link", "del", f.HostLink)
		}
		if nsCreated {
			runIPIgnoreErr(ctx, "netns", "del", f.Ns)
		}
		if lease != nil {
			_ = releasePoolSlot(lease, f.Ns, f.HostLink)
		}
	}

	if err := runIP(ctx, "netns", "add", f.Ns); err != nil {
		rollback()
		return runtime.Value{}, fmt.Errorf("net veth-pair: %w", err)
	}
	nsCreated = true

	if err := runIP(ctx, "link", "add", f.HostLink, "type", "veth", "peer", "name", f.PeerLink); err != nil {
		rollback()
		return runtime.Value{}, fmt.Errorf("net veth-pair: %w", err)
	}
	linkCreated = true

	steps := [][]string{
		{"link", "set", f.PeerLink, "netns", f.Ns},
		{"link", "set", f.HostLink, "up"},
		{"addr", "add", hostCIDR, "dev", f.HostLink},
		{"-n", f.Ns, "link", "set", f.PeerLink, "up"},
		{"-n", f.Ns, "addr", "add", peerCIDR, "dev", f.PeerLink},
	}
	for _, step := range steps {
		if err := runIP(ctx, step...); err != nil {
			rollback()
			return runtime.Value{}, fmt.Errorf("net veth-pair: %w", err)
		}
	}

	// Capture host-side identifiers needed by dispatcher-aware
	// scripts (`bpfman dispatcher get tc-ingress $pair.host_nsid
	// $pair.host_ifindex`). Both lookups are best-effort against
	// the freshly-created topology: failures here leave the
	// fields zero rather than aborting NetPair construction, so
	// the legacy address-only call sites stay working even if a
	// future kernel rejects the lookups for some reason.
	var hostIfindex uint32
	if iface, err := net.InterfaceByName(f.HostLink); err == nil {
		hostIfindex = uint32(iface.Index)
	}
	hostNsid, _ := netns.CurrentNSID()

	pair := &runtime.NetPair{
		Ns:          f.Ns,
		HostLink:    f.HostLink,
		PeerLink:    f.PeerLink,
		HostAddr:    hostAddr,
		PeerAddr:    peerAddr,
		HostIfindex: hostIfindex,
		HostNsid:    hostNsid,
	}
	rememberNetPairLease(pair, lease)
	return runtime.ValueFromNetPair(pair), nil
}

// linkNameSeq is the process-local atomic counter feeding
// uniqueLinkBase. PID plus counter is hashed to produce a 12-hex
// identifier, so two parallel processes (different PIDs) and two
// sequential calls within the same process (different counters)
// always produce distinct bases.
var linkNameSeq atomic.Uint64

// uniqueLinkBase returns a 14-character identifier suitable for use
// as the shared base of a veth-pair plus netns trio. The format is
// "B<12 hex>N" -- the leading "B" and trailing "N" are the first and
// last letters of "bpfman" so a stray name is recognisable as our
// allocation. The total length leaves exactly one character of
// headroom under Linux IFNAMSIZ (15), so a per-end suffix like "a"
// or "b" still fits the host- and peer-side veth names.
//
// Borrowed verbatim from e2e/helpers.go's uniqueTestName; the e2e
// suite and bpfman-shell now share the same name shape so a
// breadcrumb in /sys/class/net or /run/netns is attributable from
// either side.
func uniqueLinkBase() string {
	n := linkNameSeq.Add(1)
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%d", os.Getpid(), n)
	return fmt.Sprintf("B%012xN", h.Sum64()&0xffffffffffff)
}

// generateTopologyNames returns a trio of unique names suitable for
// `net veth-pair`'s auto-naming mode. The netns name is the bare
// 14-character base; the host- and peer-side veth names are the
// base with "a" and "b" suffixes (15 chars each, IFNAMSIZ-safe).
// The three names share a base so a script that prints any one of
// them can be cross-referenced to its peers visually.
func generateTopologyNames() (ns, hostLink, peerLink string) {
	base := uniqueLinkBase()
	return base, base + "a", base + "b"
}

// handleNetRelease tears the topology down in LIFO-correct order:
// delete the host-side veth first (the kernel reclaims the peer
// end with it), then delete the netns. Both steps ignore failure
// because missing resources are the desired terminal state. When
// the pair was built in auto-address mode, the builtin-private
// pool lease is released after the kernel teardown so the
// lockfile's final provenance reflects the just-torn-down
// topology. The second call against an already-released handle
// short-circuits at the lifecycle latch. The envelope is always
// OK so `defer net release $pair` never disturbs a clean exit.
func handleNetRelease(ctx context.Context, args []runtime.Arg) (runtime.Value, error) {
	if len(args) != 1 {
		return runtime.Value{}, fmt.Errorf("net release: requires exactly one $pair argument")
	}
	pair, err := netPairFromArg(args[0])
	if err != nil {
		return runtime.Value{}, fmt.Errorf("net release: %w", err)
	}
	if pair.MarkReleased() {
		return runtime.ValueFromEnvelope(runtime.OkEnvelope()), nil
	}
	runIPIgnoreErr(ctx, "link", "del", pair.HostLink)
	runIPIgnoreErr(ctx, "netns", "del", pair.Ns)
	if lease := takeNetPairLease(pair); lease != nil {
		_ = releasePoolSlot(lease, pair.Ns, pair.HostLink)
	}
	return runtime.ValueFromEnvelope(runtime.OkEnvelope()), nil
}

// NetExecEnvelope runs `ip netns exec NS CMD ARGS...` synchronously
// and captures the result. Shared between the statement-position
// dispatch through handleNet and the bind-position special case
// in makeExecBind, so both paths produce identical envelopes
// (`guard _ <- net exec $pair ping` halts the same way
// `guard _ <- ip netns exec NS ping` would).
func NetExecEnvelope(ctx context.Context, args []runtime.Arg) (runtime.Envelope, error) {
	if len(args) < 2 {
		return runtime.Envelope{}, fmt.Errorf("net exec: requires $pair and a command")
	}
	pair, err := ensureNetPair(args[0])
	if err != nil {
		return runtime.Envelope{}, fmt.Errorf("net exec: %w", err)
	}
	prefix := []runtime.Arg{
		runtime.WordArg{Text: "ip"},
		runtime.WordArg{Text: "netns"},
		runtime.WordArg{Text: "exec"},
		runtime.WordArg{Text: pair.Ns},
	}
	full := append(prefix, args[1:]...)
	cap, err := driver.RunExternal(ctx, full)
	if err != nil {
		return runtime.Envelope{}, fmt.Errorf("net exec: %w", err)
	}
	return runtime.Envelope{
		OK:     cap.ExitCode == 0,
		Code:   cap.ExitCode,
		Stdout: cap.Stdout,
		Stderr: cap.Stderr,
	}, nil
}

// handleNetStart spawns `ip netns exec NS CMD ARGS...` as a
// background job. Symmetric to the corpus pattern start CMD
// returns: the same $job handle, lifecycle through wait / kill,
// process-group containment for descendants the netns-exec
// wrapper forks. No corpus script needs this yet; it ships in v1
// so the sync/async invariant (sync verbs return envelopes,
// async verbs return jobs) holds for net from day one.
func handleNetStart(ctx context.Context, env *runtime.Env, origin string, args []runtime.Arg) (runtime.Value, error) {
	if len(args) < 2 {
		return runtime.Value{}, fmt.Errorf("net start: requires $pair and a command")
	}
	pair, err := ensureNetPair(args[0])
	if err != nil {
		return runtime.Value{}, fmt.Errorf("net start: %w", err)
	}
	tempFiles, resolved, err := resolveAdapterArgs("net start", args[1:])
	if err != nil {
		return runtime.Value{}, err
	}
	argv := append([]string{"ip", "netns", "exec", pair.Ns}, driver.ArgTexts(resolved)...)
	job, err := spawnJob(ctx, env, spawnSpec{
		Argv:      argv,
		Origin:    origin,
		TempFiles: tempFiles,
	})
	if err != nil {
		return runtime.Value{}, fmt.Errorf("net start: %w", err)
	}
	return runtime.ValueFromJob(job), nil
}

// runIP runs `ip ARGS...` and packages a non-zero exit or launch
// failure as a Go error whose message includes the argv and the
// captured stderr. Used by the net veth-pair setup steps where
// any individual failure aborts the sequence and rolls back.
func runIP(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ip", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("ip %s: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return nil
}

// runIPIgnoreErr runs `ip ARGS...` and discards stdout, stderr,
// and the exit status. Used for best-effort pre-clean against
// leftover kernel state and for the LIFO teardown in net release
// where missing resources are the desired terminal state.
func runIPIgnoreErr(ctx context.Context, args ...string) {
	cmd := exec.CommandContext(ctx, "ip", args...)
	_ = cmd.Run()
}
