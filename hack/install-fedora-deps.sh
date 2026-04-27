#!/usr/bin/env bash
#
# Install the Fedora RPMs and Go-installed tools needed to build
# go-bpfman without Nix. Mirrors the toolchain expressed in
# flake.nix's default devShell, so a Fedora developer can `dnf
# install` once and then run the same `make` targets the Nix
# devShell uses.
#
# Coverage:
#
#   build/runtime: golang git make gcc clang llvm libbpf-devel
#                  kernel-headers bpftool pkgconf-pkg-config iproute
#                  jq sqlite-devel
#   static link:   glibc-static  (required for `make STATIC=1`)
#   protobuf:      protobuf-compiler  (provides `protoc`)
#   linters:       golangci-lint ShellCheck hadolint checkmake
#   docker BPF:    docker-ce (Docker upstream repo) or moby-engine
#                  (Fedora-native). Not installed by this script;
#                  pick one and install it yourself, or use
#                  BPF_USE_HOST=1 to skip the Docker BPF path.
#
# protoc-gen-go and protoc-gen-go-grpc are not packaged in Fedora.
# The Makefile installs them into ./bin via `go install` on demand
# (the same pattern used for golangci-lint), so this script does
# not need to fetch them up front.
#
# Usage: hack/install-fedora-deps.sh
#   Re-run safely; dnf will skip already-installed packages.

set -euo pipefail

if ! command -v dnf >/dev/null; then
    echo "error: dnf not found; this script is Fedora-specific" >&2
    exit 1
fi

RPMS=(
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

sudo dnf install -y "${RPMS[@]}"

cat <<'EOF'

Fedora dependencies installed. To build without Nix on PATH:

  # If you also have Nix installed and direnv-loaded, drop its
  # entries from PATH (e.g. with nix-path-munger -d) so the Fedora
  # toolchain wins. Then:

  make            # dynamic build
  make test       # race tests
  make STATIC=1   # static link (uses glibc-static)
  make bpfman-proto  # also pulls protoc-gen-{go,go-grpc} into ./bin

EOF
