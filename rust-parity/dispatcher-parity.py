#!/usr/bin/env python3
"""Differential dispatcher parity harness.

Run the same attach/detach/unload sequences against the Go and Rust
bpfman implementations and, after each step, diff the dispatcher output.
Only the semantic core is compared -- position, priority, program name
and the proceed-on mask -- so run-specific fields (kernel ids, bpfman
link ids, pin paths, interface name, nsid/ifindex) are normalised away.
The dispatcher revision counter is reported but not asserted: it is an
internal rebuild count with no cross-implementation contract.

Both implementations expose `dispatcher list|get -o json`; the Go binary
is driven against /run/go-bpfman, the Rust binary against its default
root. Each implementation gets its own veth(s) so their dispatchers
never collide.

Run from the go-bpfman repo root:  python3 ./rust-parity/dispatcher-parity.py
(bpfman/ip calls sudo internally; passwordless sudo required.)
"""

import json
import os
import re
import subprocess
import sys

GO_BIN = "./bin/bpfman"
GO_ROOT = "/run/go-bpfman"
RUST_BIN = os.path.expanduser(
    "~/src/github.com/bpfman/bpfman/worktrees/general/target/debug/bpfman"
)
TESTDATA = "e2e/testdata/bpf"
XDP_OBJ = f"{TESTDATA}/xdp_pass.bpf.o"
XDP_SPEC = "xdp:pass"
XDP_IMG = "quay.io/bpfman-bytecode/go-xdp-counter"
XDP_IMG_SPEC = "xdp:xdp_stats"
TC_OBJ = f"{TESTDATA}/tc_counter.bpf.o"
TC_SPEC = "tc:stats"

MAX_PROGRAMS = 10


def sh(cmd, check=True):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError(f"FAILED ({r.returncode}): {cmd}\n--stderr--\n{r.stderr}\n--stdout--\n{r.stdout}")
    return r


def ok(cmd):
    """Run and return True on success, False on failure (no raise)."""
    return sh(cmd, check=False).returncode == 0


def ifindex(iface):
    return int(sh(f"cat /sys/class/net/{iface}/ifindex").stdout.strip())


class Impl:
    def __init__(self, name, rust, iface, peer):
        self.name = name
        self.rust = rust
        self.iface = iface
        self.peer = peer

    def _bpfman(self, args):
        if self.rust:
            return f"sudo {RUST_BIN} {args}"
        return f"sudo BPFMAN_RUNTIME_DIR={GO_ROOT} {GO_BIN} {args}"

    # -- program load / unload ----------------------------------------
    def load_file(self, obj, spec):
        if self.rust:
            out = sh(self._bpfman(f"load file --path {obj} --programs {spec} -m harness=parity")).stdout
            return re.search(r"Program ID:\s*(\d+)", out).group(1)
        out = sh(self._bpfman(f"program load file {obj} --programs {spec} -m harness=parity -o json")).stdout
        return str(json.loads(out)["programs"][0]["record"]["program_id"])

    def load_image(self, url, spec):
        if self.rust:
            out = sh(self._bpfman(f"load image --image-url {url} --programs {spec} -m harness=parity")).stdout
            return re.search(r"Program ID:\s*(\d+)", out).group(1)
        out = sh(self._bpfman(
            f"program load image {url} --programs {spec} --pull-policy IfNotPresent -m harness=parity -o json")).stdout
        return str(json.loads(out)["programs"][0]["record"]["program_id"])

    def unload(self, progid, check=True):
        cmd = self._bpfman(f"{'unload' if self.rust else 'program unload'} {progid}")
        return ok(cmd) if not check else bool(sh(cmd))

    # -- attach / detach ----------------------------------------------
    def _proceed(self, actions):
        if actions is None:
            return ""
        return " --proceed-on " + (" ".join(actions) if self.rust else ",".join(actions))

    def attach_xdp(self, progid, priority, proceed_on=None, iface=None, netns=None, check=True):
        iface = iface or self.iface
        ns = f" --netns {netns}" if netns else ""
        if self.rust:
            cmd = self._bpfman(
                f"attach {progid} xdp --iface {iface} --priority {priority}{self._proceed(proceed_on)}{ns} -m harness=parity")
        else:
            cmd = self._bpfman(
                f"link attach xdp {progid} {iface} --priority {priority}{self._proceed(proceed_on)}{ns} -m harness=parity -o json")
        r = sh(cmd, check=check)
        if r.returncode != 0:
            return None
        return self._link_id(r.stdout)

    def attach_tc(self, progid, direction, priority, proceed_on=None, iface=None, check=True):
        iface = iface or self.iface
        if self.rust:
            cmd = self._bpfman(
                f"attach {progid} tc --direction {direction} --iface {iface} --priority {priority}{self._proceed(proceed_on)} -m harness=parity")
        else:
            cmd = self._bpfman(
                f"link attach tc {progid} {iface} {direction} --priority {priority}{self._proceed(proceed_on)} -m harness=parity -o json")
        r = sh(cmd, check=check)
        if r.returncode != 0:
            return None
        return self._link_id(r.stdout)

    def _link_id(self, out):
        if self.rust:
            return re.search(r"Link ID:\s*(\d+)", out).group(1)
        return str(json.loads(out)["record"]["id"])

    def detach(self, linkid, check=True):
        if linkid is None:
            return
        cmd = self._bpfman(f"{'detach' if self.rust else 'link detach'} {linkid}")
        ok(cmd) if not check else sh(cmd)

    # -- dispatcher read ----------------------------------------------
    def disp(self, dtype, iface=None, nsid=None):
        """(semantic core, revision) for this impl's dispatcher, or
        (None, None) if absent. Matched by nsid when given (netns
        dispatchers), otherwise by the interface index in the host."""
        listing = json.loads(sh(self._bpfman("dispatcher list -o json")).stdout)["dispatchers"]
        if nsid is not None:
            match = next((d for d in listing if d["key"]["type"] == dtype and d["key"]["nsid"] == nsid), None)
        else:
            idx = ifindex(iface or self.iface)
            match = next((d for d in listing if d["key"]["type"] == dtype and d["key"]["ifindex"] == idx), None)
        if match is None:
            return None, None
        key = match["key"]
        snap = json.loads(sh(self._bpfman(f"dispatcher get {dtype} {key['nsid']} {key['ifindex']} -o json")).stdout)
        members = sorted(snap["members"], key=lambda m: m["position"])
        core = {
            "type": snap["key"]["type"],
            "count": len(members),
            "members": [
                {"position": m["position"], "priority": m["priority"],
                 "program_name": m["program_name"], "proceed_on": m["proceed_on"]}
                for m in members
            ],
        }
        return core, snap["revision"]


GO = Impl("go", rust=False, iface="gopar0", peer="gopar1")
RUST = Impl("rust", rust=True, iface="rustpar0", peer="rustpar1")

RESULTS = []


def check(label, dtype, iface_go=None, iface_rust=None, nsid_go=None, nsid_rust=None):
    """Assert the two implementations' dispatcher cores are equal."""
    g, gr = GO.disp(dtype, iface_go, nsid_go)
    r, rr = RUST.disp(dtype, iface_rust, nsid_rust)
    good = g == r
    RESULTS.append((label, good))
    rev = "" if gr == rr else f"   (revision go={gr} rust={rr})"
    print(f"  [{'PASS' if good else 'FAIL'}] {label}{rev if good else ''}")
    if not good:
        print("    go  :", json.dumps(g, sort_keys=True))
        print("    rust:", json.dumps(r, sort_keys=True))
        print(f"    revision go={gr} rust={rr}")


def check_bool(label, go_val, rust_val, desc=""):
    """Assert a boolean/behavioural result matches across implementations."""
    good = go_val == rust_val
    RESULTS.append((label, good))
    detail = f"  (go={go_val} rust={rust_val})" if not good else f"  ({desc}: {go_val})" if desc else ""
    print(f"  [{'PASS' if good else 'FAIL'}] {label}{detail}")


def setup_iface(iface, peer):
    if not ok(f"ip link show {iface}"):
        sh(f"sudo ip link add {iface} type veth peer name {peer}")
        sh(f"sudo ip link set {iface} up")


def teardown_iface(iface):
    sh(f"sudo ip link del {iface}", check=False)


def netns_nsid(name):
    """The net-namespace inode bpfman uses as the dispatcher nsid."""
    out = sh(f"sudo ip netns exec {name} readlink /proc/self/ns/net").stdout
    return int(re.search(r"net:\[(\d+)\]", out).group(1))


def setup_netns(name, peer):
    sh(f"sudo ip netns del {name}", check=False)
    sh(f"sudo ip netns add {name}")
    sh(f"sudo ip link set {peer} netns {name}")
    sh(f"sudo ip -n {name} link set {peer} up")


def teardown_netns(name):
    # Deleting the namespace also deletes the veth peer inside it, which
    # tears down its host-side partner.
    sh(f"sudo ip netns del {name}", check=False)


# --------------------------------------------------------------------
# Scenarios. Each cleans up the links/programs it created so the next
# starts from an empty dispatcher on the shared interface.
# --------------------------------------------------------------------

def scenario_proceed_on_xdp(p):
    print("scenario: XDP proceed-on encoding")
    for spec in (None, ["pass"], ["drop"], ["pass", "drop"],
                 ["pass", "dispatcher_return"], ["aborted", "drop", "pass", "tx", "redirect"]):
        gl, rl = GO.attach_xdp(p["go_pass"], 50, spec), RUST.attach_xdp(p["rust_pass"], 50, spec)
        check(f"xdp proceed-on {spec or 'default'}", "xdp")
        GO.detach(gl); RUST.detach(rl)


def scenario_proceed_on_tc(p):
    print("scenario: TC proceed-on encoding")
    for spec in (None, ["ok"], ["pipe"], ["ok", "shot", "dispatcher_return"], ["unspec"], ["dispatcher_return"]):
        gl, rl = GO.attach_tc(p["go_tc"], "ingress", 50, spec), RUST.attach_tc(p["rust_tc"], "ingress", 50, spec)
        check(f"tc proceed-on {spec or 'default'}", "tc-ingress")
        GO.detach(gl); RUST.detach(rl)


def scenario_ordering_xdp(p):
    print("scenario: XDP priority ordering (attach out of order)")
    g = [GO.attach_xdp(p["go_pass"], pri) for pri in (30, 10, 20)]
    r = [RUST.attach_xdp(p["rust_pass"], pri) for pri in (30, 10, 20)]
    check("xdp positions after attaching 30,10,20", "xdp")
    for l in g: GO.detach(l)
    for l in r: RUST.detach(l)


def scenario_zero_priority_xdp(p):
    print("scenario: XDP zero-priority with name tie-break")
    gl1, gl2 = GO.attach_xdp(p["go_stats"], 0), GO.attach_xdp(p["go_pass"], 0)
    rl1, rl2 = RUST.attach_xdp(p["rust_stats"], 0), RUST.attach_xdp(p["rust_pass"], 0)
    check("xdp two names at priority 0 (pass < xdp_stats)", "xdp")
    GO.detach(gl1); GO.detach(gl2); RUST.detach(rl1); RUST.detach(rl2)


def scenario_tie_break_xdp(p):
    print("scenario: XDP tie-break by program name at equal priority")
    gl1, gl2 = GO.attach_xdp(p["go_stats"], 50), GO.attach_xdp(p["go_pass"], 50)
    rl1, rl2 = RUST.attach_xdp(p["rust_stats"], 50), RUST.attach_xdp(p["rust_pass"], 50)
    check("xdp two names at priority 50 (pass < xdp_stats)", "xdp")
    GO.detach(gl1); GO.detach(gl2); RUST.detach(rl1); RUST.detach(rl2)


def scenario_slot_reuse_xdp(p):
    print("scenario: XDP slot reuse after detaching the middle member")
    g = [GO.attach_xdp(p["go_pass"], pr) for pr in (10, 20, 30)]
    r = [RUST.attach_xdp(p["rust_pass"], pr) for pr in (10, 20, 30)]
    check("xdp after attaching 10,20,30", "xdp")
    GO.detach(g[1]); RUST.detach(r[1])
    check("xdp after detaching the priority-20 member", "xdp")
    g.append(GO.attach_xdp(p["go_pass"], 25)); r.append(RUST.attach_xdp(p["rust_pass"], 25))
    check("xdp after reattaching at priority 25", "xdp")
    for l in (g[0], g[2], g[3]): GO.detach(l)
    for l in (r[0], r[2], r[3]): RUST.detach(l)


def scenario_detach_ends_xdp(p):
    print("scenario: XDP detach first, then last, rebuilding positions")
    g = [GO.attach_xdp(p["go_pass"], pr) for pr in (10, 20, 30)]
    r = [RUST.attach_xdp(p["rust_pass"], pr) for pr in (10, 20, 30)]
    GO.detach(g[0]); RUST.detach(r[0])
    check("xdp after detaching the first (priority-10) member", "xdp")
    GO.detach(g[2]); RUST.detach(r[2])
    check("xdp after detaching the last (priority-30) member", "xdp")
    GO.detach(g[1]); RUST.detach(r[1])


def scenario_detach_to_empty_xdp(p):
    print("scenario: XDP dispatcher removed after last detach")
    gl, rl = GO.attach_xdp(p["go_pass"], 50), RUST.attach_xdp(p["rust_pass"], 50)
    check("xdp single member present", "xdp")
    GO.detach(gl); RUST.detach(rl)
    check("xdp dispatcher gone after last detach (both None)", "xdp")


def scenario_max_programs_xdp(p):
    print(f"scenario: XDP {MAX_PROGRAMS}-slot cap")
    g = [GO.attach_xdp(p["go_pass"], pr) for pr in range(1, MAX_PROGRAMS + 1)]
    r = [RUST.attach_xdp(p["rust_pass"], pr) for pr in range(1, MAX_PROGRAMS + 1)]
    check(f"xdp full at {MAX_PROGRAMS} members", "xdp")
    g_over = GO.attach_xdp(p["go_pass"], MAX_PROGRAMS + 1, check=False)
    r_over = RUST.attach_xdp(p["rust_pass"], MAX_PROGRAMS + 1, check=False)
    check_bool(f"xdp {MAX_PROGRAMS + 1}th attach rejected", g_over is None, r_over is None, "rejected")
    GO.detach(g_over); RUST.detach(r_over)
    for l in g: GO.detach(l)
    for l in r: RUST.detach(l)


def scenario_multi_interface_xdp(p):
    print("scenario: XDP multi-interface independence")
    setup_iface("gopar-b", "gopar-b1")
    setup_iface("rustpar-b", "rustpar-b1")
    ga = GO.attach_xdp(p["go_pass"], 50)
    gb = GO.attach_xdp(p["go_pass"], 60, iface="gopar-b")
    ra = RUST.attach_xdp(p["rust_pass"], 50)
    rb = RUST.attach_xdp(p["rust_pass"], 60, iface="rustpar-b")
    check("xdp iface A dispatcher", "xdp")
    check("xdp iface B dispatcher independent", "xdp", iface_go="gopar-b", iface_rust="rustpar-b")
    GO.detach(ga); GO.detach(gb); RUST.detach(ra); RUST.detach(rb)
    teardown_iface("gopar-b"); teardown_iface("rustpar-b")


def scenario_tc_ingress_egress(p):
    print("scenario: TC ingress/egress independence on one interface")
    gi = GO.attach_tc(p["go_tc"], "ingress", 50)
    ge = GO.attach_tc(p["go_tc"], "egress", 50)
    ri = RUST.attach_tc(p["rust_tc"], "ingress", 50)
    re_ = RUST.attach_tc(p["rust_tc"], "egress", 50)
    check("tc-ingress dispatcher", "tc-ingress")
    check("tc-egress dispatcher independent", "tc-egress")
    GO.detach(gi); GO.detach(ge); RUST.detach(ri); RUST.detach(re_)


def scenario_fill_drain_refill_xdp(p):
    print("scenario: XDP fill, drain to empty, refill")
    g = [GO.attach_xdp(p["go_pass"], pr) for pr in (10, 20, 30, 40, 50)]
    r = [RUST.attach_xdp(p["rust_pass"], pr) for pr in (10, 20, 30, 40, 50)]
    check("xdp filled with 5", "xdp")
    for l in g: GO.detach(l)
    for l in r: RUST.detach(l)
    check("xdp drained (both None)", "xdp")
    g = [GO.attach_xdp(p["go_pass"], pr) for pr in (10, 20, 30, 40, 50)]
    r = [RUST.attach_xdp(p["rust_pass"], pr) for pr in (10, 20, 30, 40, 50)]
    check("xdp refilled matches original", "xdp")
    for l in g: GO.detach(l)
    for l in r: RUST.detach(l)


def scenario_unload_member_xdp():
    print("scenario: XDP unload a dispatcher member (cascade behaviour)")
    # Dedicated throwaway programs so unloading them cannot affect the
    # shared program ids reused by other scenarios.
    gp = GO.load_file(XDP_OBJ, XDP_SPEC)
    gs = GO.load_image(XDP_IMG, XDP_IMG_SPEC)
    rp = RUST.load_file(XDP_OBJ, XDP_SPEC)
    rs = RUST.load_image(XDP_IMG, XDP_IMG_SPEC)
    gl1 = GO.attach_xdp(gp, 10); gl2 = GO.attach_xdp(gs, 20)
    rl1 = RUST.attach_xdp(rp, 10); rl2 = RUST.attach_xdp(rs, 20)
    check("xdp two members before unload", "xdp")
    g_unload = GO.unload(gp, check=False)
    r_unload = RUST.unload(rp, check=False)
    check_bool("unload of attached member succeeds/refuses identically", g_unload, r_unload, "succeeded")
    check("xdp dispatcher state after unloading the priority-10 member", "xdp")
    # Cleanup, tolerant of whichever path each impl took.
    GO.detach(gl1, check=False); GO.detach(gl2, check=False)
    RUST.detach(rl1, check=False); RUST.detach(rl2, check=False)
    for impl, pid in ((GO, gp), (GO, gs), (RUST, rp), (RUST, rs)):
        impl.unload(pid, check=False)


def scenario_netns_xdp(p):
    print("scenario: XDP dispatcher inside a network namespace")
    setup_netns("gopar-ns", GO.peer)
    setup_netns("rustpar-ns", RUST.peer)
    gnsid, rnsid = netns_nsid("gopar-ns"), netns_nsid("rustpar-ns")
    gpath, rpath = "/var/run/netns/gopar-ns", "/var/run/netns/rustpar-ns"

    gl1 = GO.attach_xdp(p["go_pass"], 50, iface=GO.peer, netns=gpath)
    gl2 = GO.attach_xdp(p["go_stats"], 60, iface=GO.peer, netns=gpath)
    rl1 = RUST.attach_xdp(p["rust_pass"], 50, iface=RUST.peer, netns=rpath)
    rl2 = RUST.attach_xdp(p["rust_stats"], 60, iface=RUST.peer, netns=rpath)
    check("xdp netns two members", "xdp", nsid_go=gnsid, nsid_rust=rnsid)

    GO.detach(gl1); RUST.detach(rl1)
    check("xdp netns after detaching priority-50", "xdp", nsid_go=gnsid, nsid_rust=rnsid)

    GO.detach(gl2); RUST.detach(rl2)
    check("xdp netns dispatcher gone after last detach (both None)", "xdp", nsid_go=gnsid, nsid_rust=rnsid)

    teardown_netns("gopar-ns"); teardown_netns("rustpar-ns")


def main():
    print("=== setting up interfaces ===")
    setup_iface(GO.iface, GO.peer)
    setup_iface(RUST.iface, RUST.peer)

    print("=== loading shared programs ===")
    p = {
        "go_pass": GO.load_file(XDP_OBJ, XDP_SPEC),
        "go_stats": GO.load_image(XDP_IMG, XDP_IMG_SPEC),
        "go_tc": GO.load_file(TC_OBJ, TC_SPEC),
        "rust_pass": RUST.load_file(XDP_OBJ, XDP_SPEC),
        "rust_stats": RUST.load_image(XDP_IMG, XDP_IMG_SPEC),
        "rust_tc": RUST.load_file(TC_OBJ, TC_SPEC),
    }
    print("  program ids:", p)

    try:
        scenario_proceed_on_xdp(p)
        scenario_proceed_on_tc(p)
        scenario_ordering_xdp(p)
        scenario_zero_priority_xdp(p)
        scenario_tie_break_xdp(p)
        scenario_slot_reuse_xdp(p)
        scenario_detach_ends_xdp(p)
        scenario_detach_to_empty_xdp(p)
        scenario_max_programs_xdp(p)
        scenario_multi_interface_xdp(p)
        scenario_tc_ingress_egress(p)
        scenario_fill_drain_refill_xdp(p)
        scenario_unload_member_xdp()
        scenario_netns_xdp(p)
    finally:
        print("=== teardown interfaces ===")
        teardown_netns("gopar-ns")
        teardown_netns("rustpar-ns")
        teardown_iface(GO.iface)
        teardown_iface(RUST.iface)
        teardown_iface("gopar-b")
        teardown_iface("rustpar-b")
        for impl, key in ((GO, "go_pass"), (GO, "go_stats"), (GO, "go_tc"),
                          (RUST, "rust_pass"), (RUST, "rust_stats"), (RUST, "rust_tc")):
            impl.unload(p[key], check=False)

    passed = sum(1 for _, good in RESULTS if good)
    total = len(RESULTS)
    print(f"\n=== summary: {passed}/{total} checks matched ===")
    for label, good in RESULTS:
        if not good:
            print(f"  MISMATCH: {label}")
    sys.exit(0 if passed == total else 1)


if __name__ == "__main__":
    main()
