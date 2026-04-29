#!/usr/bin/env bash
#
# Cross-arch compile-and-run for the nsenter package.
#
# Compiles the nsenter test binary for the requested architecture
# using the first available cross-GCC, then runs it under QEMU
# user-mode emulation. The CC lookup tries the Nix-style triple
# first, then the distro-style triple. When a distro sysroot
# directory exists (/usr/<prefix>-linux-gnu), QEMU is pointed at
# it via -L so DT_NEEDED lookups resolve against the cross libc.
#
# Usage: test-nsenter-cross.sh <arch>
#   arch = arm64 | ppc64le | s390x
#
# Environment:
#   NSENTER_TEST_BIN  output path for the compiled test binary
#                      (default: bin/nsenter.test)
#   NSENTER_TAGS      comma-separated build tags (optional)

set -euo pipefail

if [ $# -ne 1 ]; then
    echo "usage: $0 <arch>" >&2
    exit 2
fi

goarch=$1
nsenter_test_bin=${NSENTER_TEST_BIN:-bin/nsenter.test}
nsenter_tags=${NSENTER_TAGS:-}

case $goarch in
    arm64)   prefix=aarch64;     qemu_arch=aarch64 ;;
    ppc64le) prefix=powerpc64le; qemu_arch=ppc64le ;;
    s390x)   prefix=s390x;       qemu_arch=s390x ;;
    *)
        echo "error: unsupported arch '$goarch'" >&2
        echo "  supported: arm64, ppc64le, s390x" >&2
        exit 2
        ;;
esac

cc=$(command -v "${prefix}-unknown-linux-gnu-gcc" 2>/dev/null \
    || command -v "${prefix}-linux-gnu-gcc" 2>/dev/null \
    || true)

if [ -z "$cc" ]; then
    echo "error: no cross-compiler for $goarch" >&2
    echo "  tried: ${prefix}-unknown-linux-gnu-gcc (nix)" >&2
    echo "  tried: ${prefix}-linux-gnu-gcc (distro)" >&2
    exit 1
fi

qemu_cmd=("qemu-${qemu_arch}")
sysroot=""
if [ -d "/usr/${prefix}-linux-gnu" ]; then
    sysroot="/usr/${prefix}-linux-gnu"
    qemu_cmd+=(-L "$sysroot")
fi

echo "=== nsenter: ${goarch} (CC=${cc}, exec=${qemu_cmd[*]}) ==="

tag_args=()
if [ -n "$nsenter_tags" ]; then
    tag_args=(-tags="$nsenter_tags")
fi

mkdir -p "$(dirname "$nsenter_test_bin")"
CGO_ENABLED=1 GOOS=linux GOARCH="$goarch" CC="$cc" \
    go test -c "${tag_args[@]}" -o "$nsenter_test_bin" ./ns/nsenter/
file "$nsenter_test_bin"

sudo QEMU_LD_PREFIX="$sysroot" \
    "${qemu_cmd[@]}" "$nsenter_test_bin" -test.v
