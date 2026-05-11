# Plan: the `net` built-in for e2e dispatcher-test topology fixtures

## 1. What this document is

A working design memo for `net`, a new effectful built-in in the
bpfman-shell DSL covering veth-pair / netns / addr / route setup
for TC / TCX / XDP dispatcher tests. Sibling of `fire` at the
same architectural layer: domain-specific fixture primitive, sugar
over the underlying primitives, narrow scope by construction.
Retire this file once the built-in ships and the user-facing
surface lands in `REPL-REDESIGN.md`.

Read after `PLAN-fire-builtin.md`. The architectural framing
(deep module, script chooses parameters, builtin owns mechanism,
test-intent over recipe) is shared; this document does not repeat
the rationale at length.

## 2. The current ceremony

Every dispatcher test under `e2e/new/` (TC, TCX, XDP -- nine
scripts) carries an identical 19-line block setting up a veth
pair, a netns, two `/32` addresses, two routes, and two defers
for cleanup. The shapes are uniform across the corpus: every
script uses `198.51.100.1` on the host side and `198.51.100.2`
inside the netns, only the per-test names (`ns`, `veth_a`,
`veth_b`) differ.

```
let ns      = "bpfsh-mtc"
let veth_a  = "vea-mtc"
let veth_b  = "veb-mtc"
let ip_a    = "198.51.100.1"
let ip_b    = "198.51.100.2"

# Best-effort cleanup of any leftover state from a prior failed run.
let _ <- ip link del $veth_a
let _ <- ip netns del $ns

guard _ <- ip netns add $ns
defer ip netns del $ns

guard _ <- ip link add $veth_a type veth peer name $veth_b
defer ip link del $veth_a

guard _ <- ip link set $veth_b netns $ns

guard _ <- ip link set $veth_a up
guard _ <- ip addr add "${ip_a}/32" dev $veth_a
guard _ <- ip route add "${ip_b}/32" dev $veth_a

guard _ <- ip -n $ns link set $veth_b up
guard _ <- ip -n $ns addr add "${ip_b}/32" dev $veth_b
guard _ <- ip -n $ns route add "${ip_a}/32" dev $veth_b
```

Then the test references those names through three layers: the
BPF attach (`bpfman link attach tc -i $veth_a`), the traffic
driver (`ip netns exec $ns ping -c $n ... $ip_a`), and the
defer-cleanup chain. Nine copies of this across the corpus is
~170 lines of nearly-identical setup, with the same teardown
LIFO order each time.

Unlike fire's case, the names that appear above are not pure
mechanism: the test genuinely cares which interface gets the BPF
attach and which side originates traffic. What is mechanism is
the *order* in which the `ip` commands must be issued
(netns -> veth -> assign peer -> addresses -> routes -> both
up), the idempotent pre-clean against leftover state, the
LIFO-correct defer pair, and the duplication of the same setup
shape across every test. Those are what `net` absorbs.

## 3. Proposed surface

A single effectful built-in named `net` with subcommands:

```
net veth-pair --ns=NAME --host-link=NAME --host-addr=CIDR \
              --peer-link=NAME --peer-addr=CIDR [--no-routes]

net release $handle

net exec  $handle COMMAND ARGS...    -- sync, returns Envelope
net start $handle COMMAND ARGS...    -- async, returns Job
```

`net veth-pair` returns a handle (a new Value kind, `OriginNetPair`)
that owns the netns, the veth pair, the addresses, and the routes.
The corpus block above collapses to:

```
guard pair <- net veth-pair --ns=bpfsh-mtc \
    --host-link=vea-mtc --host-addr=198.51.100.1/32 \
    --peer-link=veb-mtc --peer-addr=198.51.100.2/32
defer net release $pair
```

Routes are added by default (every existing test needs them).
`--no-routes` opts out for the future test that does not.

`net release` runs the teardown in LIFO-correct order
(delete the host-side veth, which the kernel auto-removes the
peer with; then delete the netns), with both steps idempotent so
re-running a leaked script does not error. The handle stores
enough to do this without re-reading flags.

`net exec` runs a command in the handle's netns synchronously and
returns a captured-result Envelope, mirroring the existing `exec`
builtin's shape. The current corpus uses this exclusively: `net
exec $pair ping -c $n -i 0.05 -W 1 $pair.peer_addr` reads as one
statement without the script naming the netns separately.

`net start` is the asynchronous sibling, returning a Job. It
matches `start`'s shape: process-group leader, captured streams,
`wait` / `kill` / `defer kill` compose unchanged. No script in the
corpus needs this yet, but the verb ships in v1 because the
sync/async distinction is already a deep semantic invariant of
the language (synchronous verbs return envelopes, asynchronous
verbs return jobs); making `net` honour it from day one avoids
bolting async on later with awkward semantics.

## 4. Architectural framing

```
  ip / ip netns exec     low-level network-namespace primitives
  net <subcommand>       high-level e2e topology fixture primitive
```

`net` is sugar over the same `ip(8)` commands the scripts call
today. Internally `net veth-pair` issues the same six creates and
three state changes; `net release` issues the same two deletes;
`net exec` is `ip netns exec NS CMD ARGS`. The current spellings
remain valid as a debug escape hatch -- if a test needs an
asymmetric setup `net` does not cover, dropping back to raw `ip`
calls works and is the documented path.

The deep-module move is the same as fire's: the script chooses
the topology parameters (names, addresses), the built-in owns
the imperative order, idempotency, and cleanup. The script
states what topology it needs; `net` knows how to build it.

The parallel to fire is tighter than the difference in *what*
each absorbs (mechanism vs choreography) first suggests. fire
could have decomposed into `fire syscall NAME` / `fire signal
SIG` / `fire symbol PATH`; it did not, because that
decomposition would have been abstraction for its own sake.
fire's three kinds (unlinkat, kill, uprobe) are not arbitrary
primitives -- they are the specific test-domain operations the
BPF corpus exercises. `net veth-pair` is the same kind of
narrow encoding: not "a way to compose network primitives" but
"the topology BPF dispatcher tests need". Both builtins encode
actual test-domain operations rather than theoretical
decompositions, and the narrowness is what makes each succeed
in its corpus.

The symmetry between the two builtins, once stated outright:

```
                            fire                            net
domain stimulus      deterministic syscall events    deterministic packet flow
stable identity      stable PID                      stable interface topology
managed lifecycle    helper process                  topology
narrow purpose       kernel-observable events        packet-flow setup
shape                kind-dispatched, recipe-like    recipe-shaped
escape hatch         start env BPFMAN_SHELL_MODE=... raw ip(8) / iptables / ...
```

Both are narrow-minded by construction. The escape hatch in
each row is the safety valve that lets the builtin stay narrow
without trapping the corpus.

## 5. Declarative shape

A dispatcher test reads as:

- declare the topology parameters;
- build the veth pair and namespace;
- load and attach the BPF programs to the host side;
- drive traffic from the peer side;
- assert the per-program counters.

Today's scripts state step 2 as nineteen lines of `ip`
invocations in a specific order with two defers and a
best-effort pre-clean. The collapsed form is two lines (the
`net veth-pair` and the `defer net release`); the test's actual
subject (dispatcher chaining, proceed-on semantics) is now the
proportionally larger part of the script.

## 6. The handle and its fields

`OriginNetPair` exposes the names the script will refer to
downstream:

```
$pair.ns              -- the netns name
$pair.host_link       -- the host-side veth name
$pair.peer_link       -- the peer-side veth name
$pair.host_addr       -- the host-side address, without /CIDR
$pair.peer_addr       -- the peer-side address, without /CIDR
```

These are the four names the script passes through to `bpfman
link attach`, `net exec`, and any assertions. The `/CIDR` suffix
that `ip addr add` requires is stripped from the address fields
so the script can hand them to commands like `ping` that take a
bare address; if a script needs the masked form it builds
`${pair.host_addr}/32` itself.

The handle does not expose:

- the route additions (they are not addressable surface);
- the link-state transitions (likewise);
- any IPv6 family (v1 is IPv4 only -- see scope boundary).

A `target_binary`-style absent-when-unset semantics is not
needed here: the handle is only constructed by `net veth-pair`,
and every field is populated at construction time.

Handles are immutable. `net veth-pair` constructs the complete
topology in one call and the returned handle is a stable
identity referring to it; no later `net` operation rewrites the
handle's fields. `net release` consumes the handle (after which
referring to it is a runtime error if the script tries), but
nothing mutates it in place. The same discipline `Job` uses --
constructed once, observed many times, eventually consumed --
applies here. Mutability would introduce hidden temporal
semantics that the rest of the language does not have; the
constrained recipe shape makes immutability easy.

## 7. Subcommand shape (and why this is not fire's kind dispatch)

`fire` is one verb with a kind argument because every kind
produces the same primary (a Job) under the same lifecycle
verbs (`wait`, `kill`, `defer kill`). The operations differ only
in what gets fired, not in what the script does with the result.

`net` is one verb with subcommands because the operations
themselves differ in shape: `veth-pair` constructs a handle,
`release` consumes one, `exec` consumes one and returns an
envelope. The lifecycle verbs are not shared across operations.
A kind-dispatch model would force unnatural uniformity (every
kind produces a NetPair? returns a NetPair-or-Envelope-or-Job
union?). Subcommands fit the shape of the operations.

This is the same shape `bpfman <subcommand>` uses for its
domain commands, and the existing builtin set already uses
domain-prefix conventions where appropriate. `net` joins
`bpfman` in that pattern; `fire` stays as the
kind-dispatch case.

## 8. Naming

`net` because the operations all set up or interact with
network namespaces and links. Short, unambiguous in context,
no current collision. `netns` is too narrow (we also do veth
and addresses); `network` is verbose; `tap` and `link` are
already standard terms for narrower things.

The handle is `NetPair` (`OriginNetPair`) because a paired veth
across a single netns is the only shape `net` constructs. If a
future operation builds, say, a triple-namespace topology, the
handle type for that operation is its own (`NetTriple`?) -- the
type matches the topology kind, not a generic catch-all.

## 9. Scope boundary

`net` is for paired-veth single-netns topologies used by
TC / TCX / XDP dispatcher tests. It is *not* a general network
topology builder. The following are explicitly out of scope:

- bridges, VLANs, bonds, tap / tun, dummy interfaces;
- IPv6 (v1 is IPv4 only; revisit when a test needs it);
- multiple veth pairs in one netns;
- iptables / nftables / conntrack;
- traffic shaping beyond what bpfman attaches;
- DNS / resolv.conf setup inside the netns.

If a test needs any of these, drop back to raw `ip` /
`iptables` / etc. for that specific need, composed alongside
`net veth-pair` for the common core. The escape hatch is the
existing CLI tools; the script does not lose anything by going
down a layer for the asymmetric case.

`net` exists to make the current dispatcher tests easy. It is
intentionally corpus-shaped, not future-proofed against
hypothetical topology variants. If the next test wants
something the recipe does not cover, the answer is raw `ip`
for that specific need, not a wider `net` surface. If a
recurring pattern emerges across several future tests, that is
when a second recipe verb is justified -- with the real shape
in hand, not designed for shapes that might exist.

A one-line scope comment lives at the registration site so a
future contributor reaching for "while we are here, let me add
`net bridge`" sees the boundary at the registration point:

```
// net is for paired-veth single-netns topologies used by
// TC / TCX / XDP dispatcher tests. A richer network-fixture
// surface, if needed, lives in its own subsystem, not under
// the net builtin.
```

If the registry grows beyond `veth-pair` plus the
release/exec verbs and the next addition is something
qualitatively different (a bridge, a vxlan, a tap), that is
evidence the fixture domain itself wants its own runner or
DSL.

## 10. Validation timing

v1: runtime validation in the `net` handler. Unknown
subcommand -> runtime error; missing required flag -> runtime
error; `net release` / `net exec` against a non-NetPair value
-> runtime error.

v2 (optional, defer): hoist subcommand validation, NetPair
field-access validation, and the required-flag check into the
static checker. The OriginNetPair shape registers from the
shell package the same way OriginJob does, so `$pair.host_link`
parses cleanly today; check-time validation is the next step
when the equivalent v2 work for fire lands.

## 11. Migration plan

Three commits, mirroring the fire arc:

1. *Built-in and handle:* add the effectful built-in `net` with
   subcommands `veth-pair`, `release`, `exec`, `start`. Add the
   `OriginNetPair` shape with the five exposed fields. Wire
   subcommand dispatch through a small in-handler switch
   (parallel to `bpfman`'s subcommand dispatch); the operations
   are small enough not to warrant per-subcommand handler files
   yet.

2. *Sweep e2e/new:* mechanical substitution across the nine
   `TestMultiProg{TC,TCX,XDP}_*.bpfman` scripts. The
   nineteen-line `ip` block collapses into the
   `net veth-pair` + `defer net release` pair; the ping line
   becomes `net exec $pair ping ... $pair.peer_addr`; the BPF
   attach uses `$pair.host_link`. Expected diff: ~150 lines
   removed across the corpus, no behavioural change.

3. *Check-time validation (deferred):* hoist subcommand and
   handle-access validation into the parser. May be folded into
   the first commit if it falls out cheaply.

The raw `ip(8)` spellings stay unchanged at every step. The
escape hatch is preserved by construction.

## 12. Alternatives considered

### Composable primitives instead of one veth-pair verb

`net netns NAME`, `net veth NAME PEER`, `net addr ADDR dev LINK`,
`net route CIDR dev LINK` -- the script composes them. The
argument for this shape: networking is intrinsically
compositional (an address lives on a link, a link lives in a
netns, a route uses an address), and recipe-first risks fencing
in the next divergent test.

Rejected because the cost asymmetry actually points the other
way in this DSL. Compositional primitives permanently enlarge
the language surface: multiple verbs, richer checker semantics,
more help / completion / docs to maintain, larger backward
compatibility commitment. Recipe-first costs nothing permanent
if it turns out wrong, because the raw `ip` escape hatch is
always available and a future divergent test can write

```
guard pair <- net veth-pair ...
guard _ <- ip route del ...
guard _ <- ip route add ...
```

for its specific asymmetry without forcing a decomposition of
the language. The DSL is intentionally constrained ("lifecycle
orchestration for BPF tests", not "Linux network toolkit"); a
primitive-first design optimises for generality that the corpus
explicitly does not want. The pressure to decompose can be
absorbed by `ip` for as long as the corpus stays at this shape,
and "the corpus stays at this shape" is the working assumption
the constrained-DSL framing earns us.

Composable primitives can still land as additions later if a
recurring divergent pattern emerges -- at that point we have
the real shape, not the speculative shape that "networking
branches combinatorially" worry projects. Designing for the
shape we have is the cheaper bet here even though it is the
expensive bet in less-constrained DSLs.

### A user-level `def` in `lib.bpfman`

The same option fire considered and rejected for the same
reason: `callDef` in `shell/expr.go` returns `error`, never a
`Value`, so a `def make-veth-pair(...) { ... }` body cannot
return a NetPair handle. The language gap is real and worth
addressing separately; this builtin does not unblock that
work.

### A `bpfman-shell` subcommand mode like fire's workers

Wrong shape. The fire kinds dispatch via `BPFMAN_SHELL_MODE`
because they spawn a co-resident worker process. `net` is not a
spawn; it is a sequence of `ip(8)` invocations in the parent
shell's context. No process-mode dispatch is involved.

### Implicit teardown at scope exit (instead of `defer net release`)

The Job leak handler at scope exit SIGKILLs unmanaged jobs.
A parallel mechanism could auto-release any NetPair that was
not explicitly released, the same way an unwaited Job is
detected as a leak. Rejected for v1 because it conflates
"resource was leaked" with "resource is cleaned up", and
making the cleanup implicit hides the LIFO ordering that
sometimes matters (you may want the BPF detach to run before
the link goes away). Explicit `defer net release $pair`
matches the `defer kill $work` pattern the corpus already
uses.

### Naming the handle `OriginNetns` instead of `OriginNetPair`

The netns is one of four things the handle owns (netns, two
veth links, two addresses). Naming after one of them gives a
misleading impression of what the handle is. `NetPair` is the
most descriptive of the actual shape: a paired-link single-netns
topology.
