//go:build e2e

package grpcparallel

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/vishvananda/netlink"
)

// vethIDBase carries this test process's PID (truncated to 16
// bits) into every veth name so concurrent test runs against the
// same machine never collide on interface names. vethIDCounter
// distinguishes pairs within a single run.
var (
	vethIDBase    = uint32(os.Getpid()) & 0xffff
	vethIDCounter atomic.Uint32
)

// createTestVeth creates a uniquely-named veth pair, brings the
// host-side end up, and registers cleanup via t.Cleanup. Returns
// the host-end interface name suitable as the Iface field of an
// XDP/TC/TCX AttachInfo.
//
// This is a deliberately minimal helper for the parallel gRPC
// test: no IP addresses, no separate netns, no traffic. The test
// attaches an XDP/TC/TCX program only to verify the gRPC
// lifecycle (Load/Attach/Detach/Unload); it never generates
// traffic, so the rich helpers in e2e/helpers.go (MAC stability,
// TEST-NET-2 address pool, per-pair netns, ping wrappers) are
// not needed. Reusing NewTestVethPair would require importing the
// e2e package, which is not currently possible from outside e2e
// because helpers.go references symbols defined in _test.go
// files (rootNetnsName, sharedRuntimeMode, ...).
func createTestVeth(t *testing.T) string {
	t.Helper()
	id := vethIDCounter.Add(1)
	name := fmt.Sprintf("gv-%x-%d", vethIDBase, id)
	peer := fmt.Sprintf("gp-%x-%d", vethIDBase, id)
	// IFNAMSIZ is 16 (15 visible chars). vethIDBase is at most
	// four hex chars; id is bounded by the goroutine count
	// times sub-test count. Fail loudly rather than letting
	// the kernel truncate.
	if len(name) > 15 || len(peer) > 15 {
		t.Fatalf("veth name too long: %q / %q", name, peer)
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		PeerName:  peer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("create veth %s/%s: %v", name, peer, err)
	}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	})

	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("look up veth %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("set up %s: %v", name, err)
	}
	return name
}
