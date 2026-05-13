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
package main

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// netBindShape resolves the primary Shape of `<- net SUB ARGS...`
// at static-check time. The four subcommands produce different
// kinds: veth-pair returns a NetPair handle; release and exec
// return a captured result envelope; start returns a Job handle.
// Unknown subcommands fall through to the unsealed Unknown wildcard
// so a runtime-validated typo still parses cleanly against the
// bind-target.
func netBindShape(args []shell.Expr) shell.Shape {
	if len(args) < 1 {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	sub, ok := args[0].(*shell.LiteralExpr)
	if !ok || sub.Quoted {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	switch sub.Text {
	case "veth-pair":
		return shell.KindShape(shell.OriginNetPair)
	case "release", "exec":
		return shell.KindShape(shell.OriginEnvelope)
	case "start":
		return shell.KindShape(shell.OriginJob)
	}
	return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
}

// handleNet is the registry handler for `net <subcommand> ...`.
// Subcommand dispatch is a small in-handler switch (parallel to
// the bpfman first-noun dispatch) rather than per-subcommand
// files; the operations are small enough not to warrant the
// extra fan-out.
func handleNet(c builtinCtx) (shell.Value, error) {
	if len(c.Args) == 0 {
		return shell.Value{}, fmt.Errorf("net: subcommand required (valid: exec, release, start, veth-pair)")
	}
	sub := argText(c.Args[0])
	rest := c.Args[1:]
	switch sub {
	case "veth-pair":
		return handleNetVethPair(c.Ctx, c.Pos.cite(), rest)
	case "release":
		return handleNetRelease(c.Ctx, rest)
	case "exec":
		env, err := replNetExec(c.Ctx, rest)
		if err != nil {
			return shell.Value{}, err
		}
		return shell.ValueFromEnvelope(env), nil
	case "start":
		return handleNetStart(c.Ctx, c.Env, c.Pos.cite(), rest)
	default:
		return shell.Value{}, fmt.Errorf("net: unknown subcommand %q (valid: exec, release, start, veth-pair)", sub)
	}
}

// netPairFromArg unwraps a $pair argument into the underlying
// *shell.NetPair without checking the lifecycle latch. Used by
// net release where re-release is idempotent and the latch is
// re-checked inside the release handler.
func netPairFromArg(a shell.Arg) (*shell.NetPair, error) {
	sva, ok := a.(shell.StructuredValueArg)
	if !ok {
		return nil, fmt.Errorf("expected a $pair argument, got %T", a)
	}
	if sva.Value.Kind() != shell.OriginNetPair {
		return nil, fmt.Errorf("expected a $pair argument, got a %s value", sva.Value.Kind())
	}
	pair, ok := sva.Value.Origin().(*shell.NetPair)
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
func ensureNetPair(a shell.Arg) (*shell.NetPair, error) {
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
// shell.GenerateTopologyNames fills them in at handler entry.
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
	// from shell.GenerateTopologyNames so two concurrent runs of
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
//     shell.GenerateTopologyNames).
//   - Address group: --host-addr, --peer-addr. Pass both for
//     explicit addressing, neither for pool-allocated /30.
//
// The groups are independent: explicit addresses with auto names
// (or vice versa) are both valid. Unknown flags and positional
// arguments are rejected with the offending token quoted so the
// user can correct it.
func parseVethPairFlags(args []shell.Arg) (vethPairFlags, error) {
	var f vethPairFlags
	for i := 0; i < len(args); {
		text := argText(args[i])
		switch {
		case text == "--ns":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--ns requires a value")
			}
			f.Ns = argText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--ns="):
			f.Ns = strings.TrimPrefix(text, "--ns=")
			i++
		case text == "--host-link":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--host-link requires a value")
			}
			f.HostLink = argText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--host-link="):
			f.HostLink = strings.TrimPrefix(text, "--host-link=")
			i++
		case text == "--host-addr":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--host-addr requires a value")
			}
			f.HostAddrCIDR = argText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--host-addr="):
			f.HostAddrCIDR = strings.TrimPrefix(text, "--host-addr=")
			i++
		case text == "--peer-link":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--peer-link requires a value")
			}
			f.PeerLink = argText(args[i+1])
			i += 2
		case strings.HasPrefix(text, "--peer-link="):
			f.PeerLink = strings.TrimPrefix(text, "--peer-link=")
			i++
		case text == "--peer-addr":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--peer-addr requires a value")
			}
			f.PeerAddrCIDR = argText(args[i+1])
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
func handleNetVethPair(ctx context.Context, origin string, args []shell.Arg) (shell.Value, error) {
	f, err := parseVethPairFlags(args)
	if err != nil {
		return shell.Value{}, fmt.Errorf("net veth-pair: %w", err)
	}

	// Auto-naming runs before pool acquisition so the slot's
	// provenance body records the names the next acquirer's leak
	// check will compare against. The pool's flock + released_at
	// is what protects ordering; the identity is just the label.
	if f.AutoNames {
		f.Ns, f.HostLink, f.PeerLink = shell.GenerateTopologyNames()
	}

	var lease *shell.PoolLease
	hostCIDR := f.HostAddrCIDR
	peerCIDR := f.PeerAddrCIDR
	hostAddr := f.HostAddr
	peerAddr := f.PeerAddr
	if f.AutoAddrs {
		lease, err = shell.AcquirePoolSlot(ctx, shell.PoolAcquireRequest{
			Origin:    origin,
			NsName:    f.Ns,
			LinkAName: f.HostLink,
		})
		if err != nil {
			return shell.Value{}, fmt.Errorf("net veth-pair: %w", err)
		}
		hostCIDR = lease.HostCIDR
		peerCIDR = lease.PeerCIDR
		hostAddr = lease.HostAddr
		peerAddr = lease.PeerAddr
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
			_ = shell.ReleasePoolSlot(lease, f.Ns, f.HostLink)
		}
	}

	if err := runIP(ctx, "netns", "add", f.Ns); err != nil {
		rollback()
		return shell.Value{}, fmt.Errorf("net veth-pair: %w", err)
	}
	nsCreated = true

	if err := runIP(ctx, "link", "add", f.HostLink, "type", "veth", "peer", "name", f.PeerLink); err != nil {
		rollback()
		return shell.Value{}, fmt.Errorf("net veth-pair: %w", err)
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
			return shell.Value{}, fmt.Errorf("net veth-pair: %w", err)
		}
	}

	pair := &shell.NetPair{
		Ns:       f.Ns,
		HostLink: f.HostLink,
		PeerLink: f.PeerLink,
		HostAddr: hostAddr,
		PeerAddr: peerAddr,
		Lease:    lease,
	}
	return shell.ValueFromNetPair(pair), nil
}

// handleNetRelease tears the topology down in LIFO-correct order:
// delete the host-side veth first (the kernel reclaims the peer
// end with it), then delete the netns. Both steps ignore failure
// because missing resources are the desired terminal state. When
// the handle carries a pool lease (auto-address mode), the slot
// is released after the kernel teardown so the lockfile's final
// provenance reflects the just-torn-down topology. The second
// call against an already-released handle short-circuits at the
// lifecycle latch. The envelope is always OK so
// `defer net release $pair` never disturbs a clean exit.
func handleNetRelease(ctx context.Context, args []shell.Arg) (shell.Value, error) {
	if len(args) != 1 {
		return shell.Value{}, fmt.Errorf("net release: requires exactly one $pair argument")
	}
	pair, err := netPairFromArg(args[0])
	if err != nil {
		return shell.Value{}, fmt.Errorf("net release: %w", err)
	}
	if pair.MarkReleased() {
		return shell.ValueFromEnvelope(shell.Envelope{OK: true, Code: 0}), nil
	}
	runIPIgnoreErr(ctx, "link", "del", pair.HostLink)
	runIPIgnoreErr(ctx, "netns", "del", pair.Ns)
	if pair.Lease != nil {
		_ = shell.ReleasePoolSlot(pair.Lease, pair.Ns, pair.HostLink)
	}
	return shell.ValueFromEnvelope(shell.Envelope{OK: true, Code: 0}), nil
}

// replNetExec runs `ip netns exec NS CMD ARGS...` synchronously
// and captures the result. Shared between the statement-position
// dispatch through handleNet and the bind-position special case
// in makeExecBind, so both paths produce identical envelopes
// (`guard _ <- net exec $pair ping` halts the same way
// `guard _ <- ip netns exec NS ping` would).
func replNetExec(ctx context.Context, args []shell.Arg) (shell.Envelope, error) {
	if len(args) < 2 {
		return shell.Envelope{}, fmt.Errorf("net exec: requires $pair and a command")
	}
	pair, err := ensureNetPair(args[0])
	if err != nil {
		return shell.Envelope{}, fmt.Errorf("net exec: %w", err)
	}
	prefix := []shell.Arg{
		shell.WordArg{Text: "ip"},
		shell.WordArg{Text: "netns"},
		shell.WordArg{Text: "exec"},
		shell.WordArg{Text: pair.Ns},
	}
	full := append(prefix, args[1:]...)
	cap, err := runExternal(ctx, full)
	if err != nil {
		return shell.Envelope{}, fmt.Errorf("net exec: %w", err)
	}
	return shell.Envelope{
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
func handleNetStart(ctx context.Context, env *shell.Env, origin string, args []shell.Arg) (shell.Value, error) {
	if len(args) < 2 {
		return shell.Value{}, fmt.Errorf("net start: requires $pair and a command")
	}
	pair, err := ensureNetPair(args[0])
	if err != nil {
		return shell.Value{}, fmt.Errorf("net start: %w", err)
	}
	tempFiles, resolved, err := resolveAdapterArgs("net start", args[1:])
	if err != nil {
		return shell.Value{}, err
	}
	argv := append([]string{"ip", "netns", "exec", pair.Ns}, argTexts(resolved)...)
	job, err := spawnJob(ctx, env, spawnSpec{
		Argv:      argv,
		Origin:    origin,
		TempFiles: tempFiles,
	})
	if err != nil {
		return shell.Value{}, fmt.Errorf("net start: %w", err)
	}
	return shell.ValueFromJob(job), nil
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
