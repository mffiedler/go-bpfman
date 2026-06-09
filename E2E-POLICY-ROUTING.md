# E2E Policy Routing And VPN Interference

This note records a local e2e failure mode seen while running the
`.bpfman` packet-path scripts on a host with Tailscale enabled. The
same class can affect corporate VPNs, WireGuard, OpenVPN, VRFs, or any
host that installs policy routing rules.

## Symptom

Some TC, TCX, and XDP scripts create a veth pair with one end in the
root network namespace and one peer end in a test namespace. The test
then sends traffic through the pair and checks that dispatcher or
program counters moved.

On a host with Tailscale active, a script failed before any useful
bpfman assertion:

```text
net exec $pair ping -c 5 -i 0.05 -W 1 198.51.100.x
5 packets transmitted, 0 received, 100% packet loss
```

The failure happened before bpfman had loaded or attached the programs
under test. That makes it a harness/network precondition failure, not
a link-identity or dispatcher failure.

## Cause

The veth helper installs routes that should send traffic to the peer
address through the freshly-created host-side veth. On the failing
host, `ip route get $pair.peer_addr` selected Tailscale's policy table
instead:

```text
198.51.100.x dev tailscale0 table 52 src ...
```

The peer address was therefore routed through `tailscale0`, not through
`$pair.host_link`. Packets never crossed the veth path the test had
constructed.

This is not specific to Tailscale. Any policy rule that is consulted
before the normal table and selects a tunnel/default route for the test
address can steal the reply path.

Changing the address pool is not sufficient. Against a policy-routed
default table, no documentation prefix or RFC1918 range is inherently
safe. A more-specific rule, or a topology that avoids host routing
altogether, is required.

## Current Mitigation

The `.bpfman` library has a temporary precheck:

```text
require_host_route_to_peer $pair
```

It runs:

```sh
ip route get $pair.peer_addr
```

and requires the selected route to mention `$pair.host_link`. If the
route goes through a tunnel table, the script fails immediately with a
diagnostic that names the selected route.

This does not make the test pass under hostile host routing. It turns
an opaque packet-loss failure into a clear environmental failure.

## Local Workaround

For local development, a temporary policy rule can force the e2e test
subnet back through the main table ahead of the VPN rule:

```sh
sudo ip rule add to 198.51.100.0/24 lookup main priority 5000
# run e2e
sudo ip rule del to 198.51.100.0/24 lookup main priority 5000
```

This should remain a local workaround, not harness policy. Rule
priorities are host-specific, and different VPNs may install rules at
different priorities. Injecting routes into a VPN-owned table, such as
Tailscale table 52, is worse: the VPN daemon owns and reconciles that
table.

## Preferred Fix

The robust fix is to remove host routing from the data path.

Use two dedicated test network namespaces:

- namespace A contains veth end A, where bpfman attaches TC, TCX, or
  XDP programs;
- namespace B contains veth end B, where the script sends traffic;
- each namespace has only the routes needed for the veth peer.

That topology keeps the test on real kernel networking:

- real veth devices;
- real packets crossing the veth pair;
- real qdisc/netlink TC attachment;
- real XDP/TC dispatcher programs;
- real freplace extension links;
- real namespace IDs in the dispatcher path.

It does not weaken the dispatcher or packet-path tests. It removes
accidental coverage of root-namespace host routing, which is not what
those tests are trying to prove. A small root-namespace smoke test can
cover that separately and skip when the route precondition is not met.

Do not put both veth ends in the same namespace. If both addresses are
local to one namespace, the kernel can deliver locally without forcing
the frame across the veth pair, weakening the packet-path assertion.

## Test Split

Long term, the scripts should split responsibilities:

- dispatcher and packet-path tests should use isolated two-netns veth
  pairs so VPNs and host policy routing cannot affect them;
- one small root-netns attach/traffic smoke test should keep coverage
  for attaching to a root namespace interface;
- that smoke test should check `ip route get $peer_addr` first and
  skip, not fail, when host policy routing steals the route.

The current `require_host_route_to_peer` helper is a stopgap until the
two-netns topology exists.
