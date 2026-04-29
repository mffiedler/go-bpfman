#!/usr/bin/env bash
#
# Install the Fedora RPMs needed to build, test, and lint go-bpfman.
# After running this once, every `make` target in the project
# Makefile is reachable on a stock Fedora system with no further
# setup.
#
# Coverage:
#
#   build/runtime: golang git make gcc clang llvm libbpf-devel
#                  kernel-headers bpftool pkgconf-pkg-config iproute
#                  jq sqlite-devel
#   static link:   glibc-static  (required for `make STATIC=1`)
#   protobuf:      protobuf-compiler  (provides `protoc`)
#   linters:       golangci-lint ShellCheck hadolint checkmake
#
# Not installed here:
#
#   docker:        docker-ce (from Docker's upstream repo) or
#                  moby-engine (Fedora-native). Only needed for
#                  `make build-image-dev` and friends (the runtime
#                  image build); `make bpf-build` uses the host
#                  clang/llvm/libbpf-devel/kernel-headers RPMs
#                  installed by this script.
#   protoc-gen-go,
#   protoc-gen-go-grpc:
#                  not packaged in Fedora. The Makefile installs
#                  them into ./bin via `go install` on demand,
#                  the same way it handles golangci-lint, so this
#                  script does not need to fetch them up front.
#
# Usage: hack/install-fedora-deps.sh
#   Re-run safely; dnf will skip already-installed packages.

set -euo pipefail

if ! command -v dnf >/dev/null; then
    echo "error: dnf not found; this script is Fedora-specific" >&2
    exit 1
fi

rpms=(
    bpftool
    checkmake
    clang
    gcc
    git
    glibc-static
    golang
    golangci-lint
    hadolint
    iproute
    jq
    kernel-headers
    libbpf-devel
    llvm
    make
    pkgconf-pkg-config
    protobuf-compiler
    ShellCheck
    sqlite-devel
)

# Skip sudo when already root (e.g. inside a container during
# `docker build`); use sudo on a regular host where the user
# typically isn't root.
sudo_cmd=
if [ "$(id -u)" -ne 0 ]; then
    sudo_cmd=sudo
fi

$sudo_cmd dnf install -y "${rpms[@]}"

cat <<'EOF'

Fedora dependencies installed. Common starting points:

  make                  # dynamic build of bin/bpfman
  make test             # unit tests (race detector enabled)
  make STATIC=1         # static link, requires glibc-static
  make bpfman-proto     # regenerate proto stubs
  make build-image-dev  # local docker image (bpfman:dev)
  make help             # full target list

EOF
