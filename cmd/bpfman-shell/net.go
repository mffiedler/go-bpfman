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

	"github.com/frobware/go-bpfman/shell"
)

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
		return handleNetVethPair(c.Ctx, rest)
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
// HostAddrCIDR / PeerAddrCIDR are the full prefix forms passed to
// `ip addr add` and `ip route add`; HostAddr / PeerAddr are the
// bare addresses published on the NetPair handle so the script
// can hand them to commands like ping that take an unmasked IP.
type vethPairFlags struct {
	Ns           string
	HostLink     string
	HostAddrCIDR string
	HostAddr     string
	PeerLink     string
	PeerAddrCIDR string
	PeerAddr     string
	NoRoutes     bool
}

// parseVethPairFlags walks `net veth-pair`'s arg list, accepting
// both the `--name=value` and `--name value` spellings for each
// flag (matching fire's convention). All five name/address flags
// are required; --no-routes is optional. Unknown flags and
// positional arguments are rejected with the offending token
// quoted so the user can correct it.
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
		case text == "--no-routes":
			f.NoRoutes = true
			i++
		case strings.HasPrefix(text, "--"):
			return f, fmt.Errorf("unknown flag %q", text)
		default:
			return f, fmt.Errorf("unexpected positional argument %q (net veth-pair takes only flags)", text)
		}
	}

	missing := []string{}
	if f.Ns == "" {
		missing = append(missing, "--ns")
	}
	if f.HostLink == "" {
		missing = append(missing, "--host-link")
	}
	if f.HostAddrCIDR == "" {
		missing = append(missing, "--host-addr")
	}
	if f.PeerLink == "" {
		missing = append(missing, "--peer-link")
	}
	if f.PeerAddrCIDR == "" {
		missing = append(missing, "--peer-addr")
	}
	if len(missing) > 0 {
		return f, fmt.Errorf("missing required flag(s): %s", strings.Join(missing, ", "))
	}

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
	return f, nil
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
// in the canonical order: best-effort pre-clean, create netns,
// create the veth pair, move the peer end into the netns, bring
// both ends up, assign addresses, and add the symmetric /32
// routes (unless --no-routes opts out). A mid-setup failure
// rolls the partially-built state back so a leaked script does
// not require manual `ip netns del` to recover.
func handleNetVethPair(ctx context.Context, args []shell.Arg) (shell.Value, error) {
	f, err := parseVethPairFlags(args)
	if err != nil {
		return shell.Value{}, fmt.Errorf("net veth-pair: %w", err)
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
	}

	if err := runIP(ctx, "netns", "add", f.Ns); err != nil {
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
		{"addr", "add", f.HostAddrCIDR, "dev", f.HostLink},
	}
	if !f.NoRoutes {
		steps = append(steps, []string{"route", "add", f.PeerAddrCIDR, "dev", f.HostLink})
	}
	steps = append(steps,
		[]string{"-n", f.Ns, "link", "set", f.PeerLink, "up"},
		[]string{"-n", f.Ns, "addr", "add", f.PeerAddrCIDR, "dev", f.PeerLink},
	)
	if !f.NoRoutes {
		steps = append(steps, []string{"-n", f.Ns, "route", "add", f.HostAddrCIDR, "dev", f.PeerLink})
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
		HostAddr: f.HostAddr,
		PeerAddr: f.PeerAddr,
	}
	return shell.ValueFromNetPair(pair), nil
}

// handleNetRelease tears the topology down in LIFO-correct order:
// delete the host-side veth first (the kernel reclaims the peer
// end with it), then delete the netns. Both steps ignore failure
// because missing resources are the desired terminal state, and
// the second call against an already-released handle short-
// circuits at the lifecycle latch. The envelope is always OK so
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
