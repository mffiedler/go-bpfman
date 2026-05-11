// netpool is the cross-process slot allocator backing
// `net veth-pair`'s auto-address mode. Each running bpfman-shell
// process competes for one of 64 /30 subnets carved out of
// 198.51.100.0/24 (TEST-NET-2); exclusion is via flock on a
// per-slot lockfile under /run/bpfman-net-pool/. The kernel
// releases the flock when the holder exits, so the pool is
// self-cleaning against crashes without a daemon. See
// docs/PLAN-net-auto-subnet.md for the full design.
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// PoolSize is the number of /30 subnets the pool carves out of
// 198.51.100.0/24. Sized well above realistic parallelism for
// the dispatcher corpus; widening to a second /24 doubles the
// address management cost for no near-term gain.
const PoolSize = 64

// DefaultPoolRoot is the production location of the pool, sibling
// of `/run/bpfman`. Callers that pass an empty Root in
// PoolAcquireRequest get this; tests pass a t.TempDir() so the
// production path is never written to by accident and so tests
// can run in parallel.
const DefaultPoolRoot = "/run/bpfman-net-pool"

// poolSubnet is the host part of the TEST-NET-2 /24 the pool
// allocates inside. Slot n occupies n's /30 at base 4*(n-1):
// host = base+1, peer = base+2, broadcast = base+3.
const poolSubnetPrefix = "198.51.100."

// LinkExistsFn / NetnsExistsFn are the assertSlotClean
// primitives. Callers normally leave them nil so the defaults
// (netlink.LinkByName / netns.GetFromName) run; tests pass stubs
// to model a leaked tenant without needing real kernel state.
//
// The defaults use vishvananda/netlink and vishvananda/netns
// (both already direct module deps): netlink.LinkByName issues a
// single RTM_GETLINK with NLM_F_REQUEST (targeted lookup, no
// dump, so NLM_F_DUMP_INTR cannot strike); netns.GetFromName
// opens the named netns file under /var/run/netns/. Both
// definitively distinguish presence from absence without parsing
// iproute2 output.
type LinkExistsFn func(name string) bool
type NetnsExistsFn func(name string) bool

// PoolLease is the handle returned by AcquirePoolSlot. The flock
// is held until ReleasePoolSlot closes the lockfile, so callers
// must arrange a paired release on every code path that consumes
// a lease.
//
// Slot, HostCIDR, PeerCIDR, HostAddr, and PeerAddr are exported
// because the caller plumbs them into `ip addr add` / `ip route`
// commands and onto the user-visible NetPair handle. lockFile and
// origin are private to the pool.
type PoolLease struct {
	// Slot is the 1-indexed slot number. The lockfile path is
	// derived from this in slotLockPath.
	Slot uint32

	// HostCIDR and PeerCIDR are the /30 prefix forms to pass to
	// `ip addr add`. The kernel installs the connected route
	// automatically; the pool does not manage explicit routes.
	HostCIDR string
	PeerCIDR string

	// HostAddr and PeerAddr are the bare host-side and peer-side
	// addresses (no /CIDR suffix), suitable for handing to ping
	// or for exposing on $pair.host_addr / $pair.peer_addr.
	HostAddr string
	PeerAddr string

	lockFile *os.File
	origin   string
}

// PoolAcquireRequest is the call-site context recorded in the
// slot's provenance body, plus optional overrides for the pool
// root and the two cleanliness-check functions. Production
// callers leave Root, LinkExists, and NetnsExists at their
// zero values; tests pass a t.TempDir() and stubs so they can
// run in parallel without sharing package-level state.
type PoolAcquireRequest struct {
	// Root is the on-disk pool directory. Empty defaults to
	// DefaultPoolRoot.
	Root string

	// Origin is a free-form attribution string, typically the
	// source location of the `net veth-pair` invocation
	// (file:line[:col]). Empty is tolerated but discouraged.
	Origin string

	// NsName is the netns name the caller will pass to
	// `ip netns add`. Used by assertSlotClean's netns check.
	NsName string

	// LinkAName is the host-side veth name the caller will pass
	// to `ip link add`. Used by assertSlotClean's link check.
	LinkAName string

	// LinkExists and NetnsExists override the assertSlotClean
	// primitives. Nil means "use the default kernel-truth
	// implementation". Set by tests to inject a known leak
	// without needing CAP_NET_ADMIN.
	LinkExists  LinkExistsFn
	NetnsExists NetnsExistsFn
}

// provenance is the JSON body written to a slot's lockfile. It
// records who last held the slot and when, so the next acquirer
// can attribute a leaked resource and so FIFO ordering has an
// explicit timestamp to sort on.
type provenance struct {
	Origin     string `json:"origin,omitempty"`
	NsName     string `json:"ns_name,omitempty"`
	LinkAName  string `json:"link_a_name,omitempty"`
	AcquiredAt string `json:"acquired_at,omitempty"`
	ReleasedAt string `json:"released_at,omitempty"`
}

// AcquirePoolSlot leases a slot under flock. The algorithm:
//
//  1. Scan slots [1, PoolSize], computing a sort key per slot
//     (zero time if no lockfile; ReleasedAt from the body if
//     present; mtime otherwise).
//  2. Sort ascending so unused and longest-released slots win
//     first.
//  3. Walk the sorted candidates trying flock(LOCK_EX|LOCK_NB)
//     on each. Skip any already-held slot.
//  4. Under the lock, re-read the body (state may have advanced
//     since the scan), validate cleanliness, and write a fresh
//     acquire-time body before returning.
//
// The mkdir is best-effort: a pre-existing pool root is fine; a
// permissions failure surfaces here rather than during the first
// flock.
func AcquirePoolSlot(ctx context.Context, req PoolAcquireRequest) (*PoolLease, error) {
	root := req.Root
	if root == "" {
		root = DefaultPoolRoot
	}
	linkCheck := req.LinkExists
	if linkCheck == nil {
		linkCheck = defaultLinkCheck
	}
	netnsCheck := req.NetnsExists
	if netnsCheck == nil {
		netnsCheck = defaultNetnsCheck
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("net pool: mkdir %s: %w", root, err)
	}

	cands, err := scanSlots(root)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].sortKey.Before(cands[j].sortKey)
	})

	for _, c := range cands {
		path := slotLockPath(root, c.slot)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("net pool: open %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			return nil, fmt.Errorf("net pool: flock %s: %w", path, err)
		}
		// Re-read under the lock; the scan-time body is potentially
		// stale because another process may have released and
		// re-acquired between the scan and the flock.
		prev, err := readProvenance(path)
		if err != nil {
			f.Close()
			return nil, err
		}
		if err := assertSlotClean(c.slot, prev, linkCheck, netnsCheck); err != nil {
			f.Close()
			return nil, err
		}
		now := time.Now().UTC()
		fresh := provenance{
			Origin:     req.Origin,
			NsName:     req.NsName,
			LinkAName:  req.LinkAName,
			AcquiredAt: now.Format(time.RFC3339Nano),
		}
		if err := writeProvenance(f, fresh); err != nil {
			f.Close()
			return nil, fmt.Errorf("net pool: write %s: %w", path, err)
		}
		host, peer := slotAddrs(c.slot)
		return &PoolLease{
			Slot:     c.slot,
			HostCIDR: fmt.Sprintf("%s/30", host),
			PeerCIDR: fmt.Sprintf("%s/30", peer),
			HostAddr: host,
			PeerAddr: peer,
			lockFile: f,
			origin:   req.Origin,
		}, nil
	}
	return nil, errors.New("net pool: more than 64 concurrent pairs in flight")
}

// ReleasePoolSlot writes a final provenance body carrying
// released_at, then closes the lockfile (which releases the
// flock). The teardown order matches plan section 11: the body is
// the canonical "what was released" payload, and the flock release
// must happen before any subsequent operation on the local handle
// short-circuits.
//
// nsName and linkAName are passed in so the released body carries
// the names the next acquirer will validate against; they should
// match what the caller installed in the kernel under this slot.
//
// A nil lease or a lease with Slot == 0 is a no-op (the explicit-
// address path on `net veth-pair` does not lease a slot).
func ReleasePoolSlot(lease *PoolLease, nsName, linkAName string) error {
	if lease == nil || lease.Slot == 0 || lease.lockFile == nil {
		return nil
	}
	final := provenance{
		Origin:     lease.origin,
		NsName:     nsName,
		LinkAName:  linkAName,
		ReleasedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	// Preserve AcquiredAt from the on-disk body so the released
	// record carries both timestamps for post-mortem inspection.
	if prev, err := readProvenanceFromFile(lease.lockFile); err == nil {
		final.AcquiredAt = prev.AcquiredAt
	}
	if err := writeProvenance(lease.lockFile, final); err != nil {
		lease.lockFile.Close()
		lease.lockFile = nil
		return fmt.Errorf("net pool: write final provenance: %w", err)
	}
	err := lease.lockFile.Close()
	lease.lockFile = nil
	if err != nil {
		return fmt.Errorf("net pool: close slot %d lockfile: %w", lease.Slot, err)
	}
	return nil
}

// defaultLinkCheck issues a single RTM_GETLINK and reports
// whether the kernel returned a link. Any error -- LinkNotFound,
// permission, transport -- degrades to "absent" so the next
// setup step surfaces the real problem rather than letting an
// unrelated environment fault masquerade as a leak.
func defaultLinkCheck(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}

// defaultNetnsCheck opens /var/run/netns/NAME; the open succeeds
// iff the named netns is currently mounted. The fd is closed
// immediately because the netns is only needed for the existence
// signal.
func defaultNetnsCheck(name string) bool {
	h, err := netns.GetFromName(name)
	if err != nil {
		return false
	}
	h.Close()
	return true
}

// slotCandidate carries the sort key and previous provenance for
// a slot during the acquire scan.
type slotCandidate struct {
	slot    uint32
	sortKey time.Time
	prev    provenance
}

// scanSlots inspects every slot lockfile and returns one
// slotCandidate per slot. Missing files use the zero time (oldest
// possible, so unused slots win first). Files with a parseable
// ReleasedAt use that timestamp. Everything else falls back to
// mtime, covering legacy bodies, crash-leaked bodies, and bodies
// written by a process that never made it past AcquiredAt.
func scanSlots(root string) ([]slotCandidate, error) {
	out := make([]slotCandidate, 0, PoolSize)
	for slot := uint32(1); slot <= PoolSize; slot++ {
		path := slotLockPath(root, slot)
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, slotCandidate{slot: slot})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("net pool: stat %s: %w", path, err)
		}
		prev, _ := readProvenance(path)
		key := time.Time{}
		if prev.ReleasedAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, prev.ReleasedAt); err == nil {
				key = t
			}
		}
		if key.IsZero() {
			key = info.ModTime()
		}
		out = append(out, slotCandidate{slot: slot, sortKey: key, prev: prev})
	}
	return out, nil
}

// assertSlotClean fails the acquire if the previous tenant
// crashed before releasing the slot AND its kernel artefacts are
// still present. The check is scoped to the crash case
// (prev.ReleasedAt == "") because a slot that recorded
// released_at is a strong promise that the release path deleted
// the named resources before writing the timestamp; any extant
// resource with the same name today was created by a later
// unrelated process (e.g. a concurrent run of a deterministically-
// named script in the test corpus) and is not a leak this
// acquirer should attribute. Treating that as a leak produces
// false positives whenever the same script runs back-to-back on
// the same host.
//
// Both checks remain name-targeted (no link-table dumps) so
// NLM_F_DUMP_INTR cannot strike under parallel churn.
func assertSlotClean(slot uint32, prev provenance, linkCheck LinkExistsFn, netnsCheck NetnsExistsFn) error {
	if prev.ReleasedAt != "" {
		return nil
	}
	if prev.LinkAName != "" && linkCheck(prev.LinkAName) {
		return leakError(slot, prev, "link", prev.LinkAName)
	}
	if prev.NsName != "" && netnsCheck(prev.NsName) {
		return leakError(slot, prev, "netns", prev.NsName)
	}
	return nil
}

// leakError formats an attributed leak message naming the previous
// tenant, when the slot was last released (or that it never was),
// and which resource is still present. The next test fails as a
// canary rather than letting the leak propagate into a mystery
// EEXIST elsewhere.
func leakError(slot uint32, prev provenance, kind, name string) error {
	when := prev.ReleasedAt
	if when == "" {
		if prev.AcquiredAt != "" {
			when = "never released (acquired " + prev.AcquiredAt + ")"
		} else {
			when = "never released"
		}
	} else {
		when = "released " + when
	}
	origin := prev.Origin
	if origin == "" {
		origin = "<unknown caller>"
	}
	return fmt.Errorf("net pool: slot %d still has %s %q from previous tenant %s (%s)", slot, kind, name, origin, when)
}

// slotLockPath returns the on-disk lockfile path for a slot,
// zero-padded to two digits so `ls` orders the files numerically.
func slotLockPath(root string, slot uint32) string {
	return filepath.Join(root, fmt.Sprintf("%02d.lock", slot))
}

// slotAddrs returns the host and peer bare-address strings for a
// slot. Layout: slot n occupies the /30 at base 4*(n-1); host is
// base+1, peer is base+2.
func slotAddrs(slot uint32) (host, peer string) {
	base := 4 * (slot - 1)
	return fmt.Sprintf("%s%d", poolSubnetPrefix, base+1), fmt.Sprintf("%s%d", poolSubnetPrefix, base+2)
}

// readProvenance loads the slot body from path, ignoring parse
// errors so a malformed legacy body is treated as empty and falls
// back to mtime-based ordering.
func readProvenance(path string) (provenance, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return provenance{}, fmt.Errorf("net pool: read %s: %w", path, err)
	}
	var p provenance
	if len(body) > 0 {
		_ = json.Unmarshal(body, &p)
	}
	return p, nil
}

// readProvenanceFromFile is the under-the-lock variant: the caller
// already holds the open fd, so we seek to start and decode
// without reopening the file. Parse failures degrade to the empty
// provenance the same way readProvenance does.
func readProvenanceFromFile(f *os.File) (provenance, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return provenance{}, err
	}
	body, err := readAll(f)
	if err != nil {
		return provenance{}, err
	}
	var p provenance
	if len(body) > 0 {
		_ = json.Unmarshal(body, &p)
	}
	return p, nil
}

// readAll drains the file into a byte slice. It exists as a small
// helper so writeProvenance and readProvenanceFromFile do not
// reach for io.ReadAll directly and bring an extra import.
func readAll(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, info.Size())
	_, err = f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, fs.ErrClosed) {
		return buf, err
	}
	return buf, nil
}

// writeProvenance truncates the lockfile, rewinds, and writes the
// JSON body. The fd is held under flock so no concurrent writer
// can interleave; the truncate+rewrite is atomic from external
// observers' perspective.
func writeProvenance(f *os.File, p provenance) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	return enc.Encode(p)
}
