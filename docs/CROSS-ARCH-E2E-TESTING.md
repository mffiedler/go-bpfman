# Cross-Architecture E2E Testing via QEMU System Emulation

## Status

Design document. Not yet implemented.

## Problem

The e2e tests exercise the full bpfman stack: loading BPF programs,
attaching to kernel hooks (XDP, TC, kprobes, uprobes, tracepoints),
reading BPF maps, and managing dispatchers. These tests require a
real Linux kernel with BPF support and run as root.

Today the e2e tests only run on amd64. The nsenter cross-arch tests
prove the CGO constructor compiles and runs on foreign architectures,
but QEMU user-mode does not virtualise the kernel, so setns, BPF
syscalls, and all kernel hook paths remain untested on non-amd64.

We want to test the real kernel path on foreign architectures, not
merely prove compilation.

## Native runners vs QEMU

GitHub now offers Linux arm64 hosted runners. For arm64, a native
runner can run the e2e suite directly without QEMU, which is simpler
and faster. QEMU system emulation remains relevant primarily for
ppc64le and s390x, which have no hosted runner offering.

The QEMU harness described here is therefore most valuable for
ppc64le and s390x. For arm64, the preferred path in CI is a native
runner; the QEMU path serves as a fallback and as the local
development option for arm64 testing on an amd64 host.

## Decisions

### D1: Guest model -- test appliance, not VM image

The guest is a one-shot test appliance, not a general-purpose VM
image. It boots from a target kernel plus a generated initramfs.
That initramfs may be assembled from a minimal distro rootfs or
from extracted distro packages, but in all cases the result is a
non-interactive, throwaway execution environment. There is no
package manager, no persistent storage, no SSH, and no build
tooling inside the guest. The guest boots, runs tests, emits
results, and powers off.

### D2: Kernel source -- prebuilt distro kernels

Initial implementation uses prebuilt distro kernels (Fedora RPMs or
Debian packages) for each target architecture. These are readily
available and likely to include the required BPF config options.
Building kernels from source remains a fallback if a required config
or boot format is unavailable for a particular architecture.

### D3: Libc and tool strategy -- ABI-matched runtime closure plus packaged tools

The guest userspace starts from the runtime closure of the produced
test binaries (dynamic linker, shared libraries) and extends it
with the packaged userspace tools required by the e2e suite. The
cross-compilers in CI link against glibc, so the guest needs the
matching glibc runtime from the cross-compilation sysroot.

On NixOS, the runtime closure can be derived from the Nix store
paths. In CI, it can be extracted from the cross-compilation
libc-dev packages already installed for the build. Tools like
lddtree or readelf can mechanically enumerate the required shared
objects.

### D4: Build location -- host only

All artefacts are built on the host. Nothing is compiled inside the
guest. BPF objects are architecture-independent and built once.
Test binaries and helper executables are cross-compiled on the host
for the target architecture. The guest is a pure execution
environment: boot, run, report.

### D5: Scope -- staged rollout

- **Phase 1:** Full e2e on arm64 using a native GitHub-hosted arm64
  runner. No QEMU needed. Proves the full stack on a second
  architecture with minimal infrastructure.
- **Phase 2:** QEMU system emulation harness for s390x. Proves the
  harness works end to end on an architecture with no native runner.
- **Phase 3:** Decide whether full e2e on ppc64le via QEMU is worth
  the CI time, or whether a smoke subset is sufficient.

### D6: Initial guest construction -- minimal distro rootfs

Initial implementation uses a minimal distro rootfs (Fedora or
Debian for the target architecture) repackaged as an initramfs.
This is the fastest route to a working harness because iproute2,
iputils, procps, and glibc come from standard packages without
manual curation. Selective package extraction remains an
optimisation path for later.

## Guest runtime requirements

The guest must include not only the cross-compiled test binary and
matching libc, but also every userspace tool invoked by the tests.
The guest image is a complete runtime environment for the test
suite, not merely a container for the test binary.

The key invariant:

> The guest image must contain the full runtime closure of the e2e
> suite: test binaries, helper executables, BPF artefacts, shared
> libraries, and all command-line tools invoked by the tests.

### Audited external command manifest

The following tools are invoked by the e2e test suite via
exec.Command or os.StartProcess (audited from e2e/*.go):

| Tool          | Usage                                      | Package    |
|---------------|--------------------------------------------|------------|
| `ip`          | veth creation, netns, addr, link, route,   | iproute2   |
|               | neigh, monitor                             |            |
| `tc`          | TC filter inspection (dispatcher tests)    | iproute2   |
| `ping`        | traffic generation for counter tests       | iputils    |
| `sysctl`      | network sysctl tuning in namespaces        | procps or busybox |
| `/bin/sh`     | runCommand() helper                        | busybox or distro shell |
| `mount`       | init script (proc, sys, bpf, tmpfs, dev)   | busybox or util-linux |

This manifest must be maintained as tests evolve. New shell-outs in
e2e/ should be added to the manifest in the same change. A future
guardrail: a script that greps for exec.Command / os.StartProcess
and compares the named tools against this manifest.

### Architecture-specific payload (cross-compiled for target)

- `e2e.test` -- the cross-compiled e2e test binary
- `call_malloc` and any other target-arch helper executables used
  by the e2e suite
- Dynamic linker and shared libraries for all guest executables
- `iproute2` (`ip`, `tc`) for the target architecture
- `iputils-ping` (`ping`) for the target architecture
- Shell and basic utilities (`/bin/sh`, `mount`, `sysctl`) from the
  distro rootfs or busybox

When using D6 (minimal distro rootfs), these tools come from
standard distro packages. Any helper executable run inside the
guest must be built for the target architecture and shipped with a
matching runtime ABI.

### Architecture-independent payload (built once on host)

- BPF object files (`e2e/testdata/bpf/*.bpf.o`)
- The `/init` script

The BPF instruction set is architecture-independent, so the same
BPF object files can generally be loaded on different CPU
architectures, assuming kernel feature parity and compatible BTF
expectations.

### Runtime environment assumptions

The guest runs the e2e suite as root, so tools such as `ping` do
not rely on setuid or file capabilities.

The guest must provide:

- Writable `/tmp`
- `/sys/fs/bpf` mounted as bpffs
- `/dev` populated via devtmpfs
- `/proc/sys` visible for sysctl operations
- `PATH` set to include tool locations

## Kernel requirements

Split by purpose.

### Required for harness boot

- CONFIG_DEVTMPFS, CONFIG_DEVTMPFS_MOUNT
- CONFIG_PROC_FS, CONFIG_SYSFS
- CONFIG_TMPFS
- CONFIG_VIRTIO, CONFIG_VIRTIO_CONSOLE (QEMU virtio serial)

### Required for current e2e test coverage

- CONFIG_BPF, CONFIG_BPF_SYSCALL
- CONFIG_BPF_JIT
- CONFIG_NET_CLS_BPF (TC classifier)
- CONFIG_NET_SCH_CLSACT (clsact qdisc for TC)
- CONFIG_VETH (e2e tests create veth pairs)
- CONFIG_KPROBES, CONFIG_UPROBES, CONFIG_TRACEPOINTS
- CONFIG_BPF_EVENTS (perf events for kprobes/uprobes)
- CONFIG_DEBUG_INFO_BTF (BTF for CO-RE)
- CONFIG_NET_ACT_BPF (TC action)

### Nice to have / future-proofing

- CONFIG_CGROUP_BPF
- CONFIG_XDP_SOCKETS (only if tests use AF_XDP)

## QEMU invocation

Each architecture has different boot parameters.

### arm64

```
qemu-system-aarch64 \
  -machine virt -cpu max \
  -kernel vmlinuz-arm64 \
  -initrd initramfs-arm64.cpio.gz \
  -nographic -serial mon:stdio \
  -m 512M -no-reboot \
  -append "console=ttyAMA0 panic=-1"
```

### ppc64le

```
qemu-system-ppc64 \
  -machine pseries -cpu power9 \
  -kernel vmlinuz-ppc64le \
  -initrd initramfs-ppc64le.cpio.gz \
  -nographic -serial mon:stdio \
  -m 512M -no-reboot \
  -append "console=hvc0 panic=-1"
```

### s390x

```
qemu-system-s390x \
  -machine s390-ccw-virtio \
  -kernel vmlinuz-s390x \
  -initrd initramfs-s390x.cpio.gz \
  -nographic -serial mon:stdio \
  -m 512M -no-reboot \
  -append "console=ttysclp0 panic=-1"
```

## Init script

The `/init` script is the only code that runs inside the guest. It
prepares the minimal environment, runs preflight checks, executes
the tests, and reports the result over the serial console.

```sh
#!/bin/sh
mkdir -p /dev /proc /sys /tmp /sys/fs/bpf

mount -t devtmpfs devtmpfs /dev
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t tmpfs tmpfs /tmp

export PATH=/bin:/sbin:/usr/bin:/usr/sbin

# Emit boot sentinel as early as possible so the host knows
# /init started, even if later steps hang.
echo "E2E_GUEST_START"
uname -a

mount -t bpf bpf /sys/fs/bpf || {
  echo "PREFLIGHT_FAIL: mount bpffs"
  echo "TEST_EXIT_CODE=99"
  poweroff -f
}

ip link set lo up

# Harness preflight: verify required tools exist.
for tool in ip tc ping sysctl; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "PREFLIGHT_FAIL: $tool not found"
    echo "TEST_EXIT_CODE=99"
    poweroff -f
  }
done

# Log tool versions for debugging guest tool skew.
ip -V 2>&1 || true
tc -V 2>&1 || true
ping -V 2>&1 || true

# Kernel feature preflight: verify veth creation works.
ip link add preflight0 type veth peer name preflight1 || {
  echo "PREFLIGHT_FAIL: veth creation"
  echo "TEST_EXIT_CODE=99"
  poweroff -f
}
ip link del preflight0 2>/dev/null

cd /test
./e2e.test -test.v -test.count=1 -test.failfast
EXIT=$?

echo "TEST_EXIT_CODE=$EXIT"
poweroff -f
```

## Host-side runner

The host-side script launches QEMU and interprets the result. It
must distinguish:

- **Test pass/fail:** `TEST_EXIT_CODE=0` or `TEST_EXIT_CODE=N`
- **Preflight failure:** `PREFLIGHT_FAIL: <reason>`, exit code 99
- **Guest panic or hang:** QEMU exits without `TEST_EXIT_CODE`
- **QEMU launch failure:** QEMU itself fails to start

Exit code 99 is reserved for harness and preflight failures. All
other exit codes come from the test binary.

The host runner preserves the following for every run:

- Full QEMU serial log (all guest output)
- QEMU command line used
- Kernel version and source metadata
- Initramfs manifest (list of files and sizes)
- `file` output for the produced test binary
- `readelf -d` or `lddtree` output showing dynamic dependencies

A failing CI run must be debuggable without re-running locally.

## Build pipeline

The pipeline has four distinct stages. Each has a name so CI can
call the lower-level pieces directly if needed.

### Stage 1: build-cross-test-binary

Cross-compile the e2e test binary and any helper executables for
the target architecture.

```
CGO_ENABLED=1 GOOS=linux GOARCH=s390x CC=s390x-linux-gnu-gcc \
  go test -c -tags=e2e -o e2e-s390x.test ./e2e
```

### Stage 2: build-guest-bundle

Assemble the initramfs: minimal distro rootfs for the target, init
script, test binary, BPF objects. Log the initramfs manifest.

```
scripts/build-guest-bundle.sh s390x e2e-s390x.test
```

### Stage 3: run-qemu-e2e

Boot QEMU, capture serial output, extract exit code, preserve logs.

```
scripts/run-qemu-e2e.sh s390x
```

### Stage 4: test-e2e-<arch> (convenience target)

```makefile
test-e2e-s390x: build-cross-test-binary-s390x build-guest-bundle-s390x
	scripts/run-qemu-e2e.sh s390x
```

## Performance expectations

QEMU system emulation without KVM is pure software emulation.
Expect materially slower execution, especially on s390x and
ppc64le. Timeouts should be set from measured data after the first
working prototype, not from assumed ratios. The e2e tests take
approximately 10 seconds on native amd64; the cross-arch equivalent
may take minutes.

Phase 2 should include a timing measurement step to establish
baseline expectations for each architecture.

## Assumptions

- The guest kernel must support all hook types the tests exercise.
- The tests must not assume host-specific filesystem layout.
- The tests must not depend on services absent in the guest (no
  systemd, no dbus, no container runtime).
- The guest userspace tool set must cover every shell-out path in
  the tests, as captured by the audited command manifest.
- The e2e suite may need small adjustments to make timeouts and
  environment assumptions explicit under emulation.
- The current test infrastructure assumes a single network namespace
  with veth pairs; the guest init must prepare this correctly.

## Risks

### Kernel config mismatch

A distro kernel may lack a required BPF option. Mitigation:
preflight checks in the init script that fail fast with a clear
message before running any tests.

### Libc version skew

The cross-compiler's glibc version must match the runtime in the
guest. Mitigation: derive the runtime from the same sysroot the
compiler uses, or use the same distro version for both.

### Guest tool behaviour skew

The versions of ip, tc, ping, or sysctl in the guest may differ
from those used in native CI or local development. Mitigation: use
stable distro packages and log tool versions in preflight output.

### Serial parsing fragility

If the guest crashes or hangs before emitting TEST_EXIT_CODE, the
host runner must distinguish explicit test failure from harness
failure from guest panic from QEMU launch failure. Mitigation:
distinct sentinel prefixes (PREFLIGHT_FAIL, TEST_EXIT_CODE,
E2E_GUEST_START) and a host-side timeout that reports "guest did
not produce a result".

### QEMU bugs

System emulation for s390x and ppc64le is less exercised than
amd64/arm64. Some BPF JIT behaviour may differ under emulation.
Mitigation: start with s390x to find issues early.

### CI time budget

Slow emulation may make the full e2e suite impractical on all
architectures. Mitigation: staged rollout with smoke subsets for
the slowest architectures.

## Prior art

- **cilium/ebpf:** Uses vimto (lmb.io/vimto) for QEMU-based kernel
  testing. Shares host filesystem via 9p. Only supports amd64 and
  arm64 with KVM. Does not support cross-arch emulation.

- **Linux kernel selftests:** The kernel's BPF selftests run under
  QEMU via virtme-ng and similar tools.

- **bpfman/bpfman (Rust):** Cross-compilation only, no QEMU system
  emulation testing.
