# ============================================================================
# Variables
# ============================================================================

# ---------------------------------------------------------------------------
# Make helpers for use inside $(if) expansions and tag joining.
# ---------------------------------------------------------------------------
comma := ,
empty :=
space := $(empty) $(empty)
# comma-join turns a space-separated word list into a comma-separated
# string, dropping empty words. Used to compose -tags lists without
# producing stray leading/trailing commas when a contributor (STATIC,
# EXTRA_TAGS) is empty.
comma-join = $(subst $(space),$(comma),$(strip $(1)))

# ---------------------------------------------------------------------------
# Tool versions -- single source of truth for CI and Docker builds.
# ---------------------------------------------------------------------------
FEDORA_VERSION ?= 43
GO_VERSION ?= 1.25
GOLANGCI_LINT_VERSION ?= v2.11.2
PROTOC_GEN_GO_VERSION ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.1

# ---------------------------------------------------------------------------
# Paths.
# ---------------------------------------------------------------------------
BIN_DIR ?= bin
COVERAGE_DIR ?= .coverage
COVERAGE_PROFILE ?= $(COVERAGE_DIR)/coverage.out
COVERAGE_HTML ?= $(COVERAGE_DIR)/coverage.html
BPFMAN_PROTO_DIR := proto
BPFMAN_PB_DIR := server/pb
DOC_PORT ?= 6060

# ---------------------------------------------------------------------------
# Image-building tool (docker / podman). Mirrors the bpfman-operator
# Makefile's convention: detect whichever of docker/podman is on PATH
# (docker wins if both are present, as CI runs on docker), fall back
# to a literal "docker" if neither is, and let the caller override
# with `make OCI_BIN=podman ...`. Exported so helper scripts the
# recipes shell out to (scripts/test-grpc.sh, etc.) pick the same
# tool up.
#
# Differences from the operator's exact form, both forced on us by
# the canonical multi-arch build flow: `make bpfman-compile` is
# re-entered inside Dockerfile.bpfman's fedora-minimal builder
# stage, which ships neither docker, podman, nor `basename`.
#
#   * stderr is redirected on both `which` legs. The operator only
#     suppresses the first; in fedora-minimal that leaks "which:
#     command not found" from the second probe on every parse.
#   * `$(notdir ...)` rather than `$(shell basename ...)`. With
#     both tools absent OCI_BIN_PATH is empty, and `basename` with
#     no operand prints "missing operand" on every reference;
#     $(notdir) is a make builtin, handles the empty case silently,
#     and never forks a shell.
#   * `$(or ...,docker)` guarantees a non-empty value. Recipes that
#     would otherwise expand to `" build -t ..."` (and confuse sh
#     with an empty argv[0]) now expand to `"docker build -t ..."`
#     and fail with the obvious "docker: command not found" if the
#     binary truly isn't there.
# ---------------------------------------------------------------------------
OCI_BIN_PATH := $(shell which docker 2>/dev/null || which podman 2>/dev/null)
OCI_BIN ?= $(or $(notdir $(OCI_BIN_PATH)),docker)
export OCI_BIN

# ---------------------------------------------------------------------------
# Image names and deployment knobs.
# ---------------------------------------------------------------------------
# BPFMAN_IMG matches the variable name used by the upstream Rust
# bpfman repository and the bpfman-operator: a single full pullspec
# (registry/repository:tag) rather than separate name + tag knobs.
# To override, pass the entire ref, e.g.
#   make build-image BPFMAN_IMG=ttl.sh/me/bpfman-test:debug
BPFMAN_IMG ?= quay.io/bpfman/bpfman:latest
CSI_SANITY_IMG ?= csi-sanity:latest

# ---------------------------------------------------------------------------
# CI build environment knobs. The `ci-*` make targets drive a
# Fedora-based docker image that mirrors what the GH workflows
# run, so a developer can reproduce CI locally with `make ci`.
# ---------------------------------------------------------------------------
CI_IMAGE       ?= bpfman-ci
CI_DOCKERFILE  ?= Dockerfile.ci
CI_E2E_BUNDLE  ?= ./ci-e2e-bundle

# Refuse to proceed if CI_E2E_BUNDLE is empty or a path that
# would `rm -rf` something catastrophic. The ci-test-e2e and
# clean recipes both $(RM) -r this directory; an unguarded
# `make clean CI_E2E_BUNDLE=.` would shred the source tree.
ifeq (,$(strip $(CI_E2E_BUNDLE)))
$(error CI_E2E_BUNDLE is empty)
endif
ifneq (,$(filter . ./ .. ../ /,$(strip $(CI_E2E_BUNDLE))))
$(error CI_E2E_BUNDLE=$(CI_E2E_BUNDLE) is unsafe (would remove source tree or filesystem root))
endif

# Caller-supplied buildx flags appended to the buildx-driven
# ci-* recipes. Empty by default for local invocations; CI sets
#   CI_BUILDX_CACHE=--cache-from type=gha,scope=ci --cache-to type=gha,mode=max,scope=ci
# (typically via `env:` in the workflow YAML) so the buildkit
# layer cache is shared across all CI jobs.
CI_BUILDX_CACHE ?=

# Shared docker-run incantation for the ci-* targets that drive
# work inside the CI container. Mounts the source tree and named
# volumes for Go's build and module caches so consecutive runs
# benefit from incremental compile.
CI_RUN := $(OCI_BIN) run --rm \
	-v $(CURDIR):/src -w /src \
	-v bpfman-ci-go-build:/root/.cache/go-build \
	-v bpfman-ci-go-mod:/root/go/pkg/mod \
	$(CI_IMAGE)

# ---------------------------------------------------------------------------
# Test knobs.
# ---------------------------------------------------------------------------
PARALLEL ?=
# Optional regex passed to `-test.run` in test-e2e / test-e2e-scripts
# to narrow which tests execute. Empty by default = run all.
TEST ?=
# Iteration count threaded into `-test.count` for `make test-e2e`.
# Default 1 keeps the local loop fast; CI pins this to 5 so every PR
# runs a small count loop on top of the deterministic gate.
STRESS_COUNT ?= 1

# ---------------------------------------------------------------------------
# Verbose-build switch, modelled on the Linux kernel tree's V=
# convention. Quiet by default (one short tag per recipe, e.g.
# `  CLANG-BPF dispatcher/bpf/tc_dispatcher.bpf.o`); `make V=1`
# restores the full command lines for debugging. Used in the BPF
# compile rules; other recipes (go fmt / vet / build) print their
# own progress and are left as-is.
# ---------------------------------------------------------------------------
V ?=
ifeq ($(V),1)
Q :=
quiet_cmd = @:
else
Q := @
quiet_cmd = @printf "  %-9s %s\n" "$(1)" "$(2)"
endif

# ---------------------------------------------------------------------------
# Static linking is opt-in via STATIC=1. Any other value disables it.
# The upstream container image enables it because the runtime base is
# scratch, which ships no libc; downstream consumers building with a
# FIPS Go toolchain (Red Hat go-toolset, Microsoft Go FIPS) must leave
# it off, since FIPS crypto requires dynamic linkage to a validated
# OpenSSL.
#
# Normalisation is required because Make's $(if cond,...) treats any
# non-empty string as true, so without the filter below STATIC=0 would
# enable static linking. `override` is required because command-line
# assignments (make STATIC=0) otherwise win over file-level ones. The
# `?=` gives STATIC an empty default so `make --warn-undefined-variables`
# does not flag the $(STATIC) reference on the next line when STATIC
# is not set in the environment or on the command line.
# ---------------------------------------------------------------------------
STATIC ?=
override STATIC := $(filter 1,$(STATIC))

# RACE=1 enables the race detector for unit, e2e test binaries, and
# bpfman-compile. Default off: race overhead can mask
# the kernel-timing behaviour e2e tests aim to surface, and on the
# static-glibc devshell -race forces external linkage. Empty default
# (rather than unset) keeps `make --warn-undefined-variables` quiet.
RACE ?=
override RACE := $(filter 1,$(RACE))

# ISOLATED_RUNTIME=1 sets BPFMAN_E2E_ISOLATED_RUNTIME=1 in the e2e
# sudo command line, switching the suite from its production-shaped
# default (one bpffs mount, one sqlite store, one manager instance
# shared across tests) to per-test isolated runtimes (each test
# gets its own). Use the isolated lane when chasing a specific
# feature where orthogonal cross-test contention would muddy
# attribution; CI exercises both lanes, so the default just decides
# which one a developer hits first when they type `make test-e2e`.
# The Go side checks for the literal string "1", so any other value
# collapses to empty here and matches the env-unset (= shared)
# behaviour. Same filter-1 pattern as RACE/STATIC.
ISOLATED_RUNTIME ?=
override ISOLATED_RUNTIME := $(filter 1,$(ISOLATED_RUNTIME))

# ---------------------------------------------------------------------------
# Runtime image dispatch.
#
# Canonical (build-image): a static binary has no runtime libc
# dependency, so it can ship on ubi9-minimal (RHEL CVE feed,
# OpenShift ecosystem). A dynamic binary needs a runtime whose
# glibc matches the Fedora builder, so it ships on fedora-minimal
# pinned to the same FEDORA_VERSION. Override RUNTIME_IMAGE on the
# command line to pin a specific tag or digest.
#
# Dev (build-image-dev): always fedora-minimal at the same
# FEDORA_VERSION, regardless of STATIC. The dev image exists for
# live in-cluster debuggability -- microdnf install whatever you
# need at the moment -- and Fedora's package set is the broadest
# minimal-distro option for ad-hoc tooling. STATIC still controls
# the host build's linkage; only the runtime base is pinned.
# ---------------------------------------------------------------------------
RUNTIME_IMAGE     ?= $(if $(STATIC),registry.access.redhat.com/ubi9/ubi-minimal:latest,registry.fedoraproject.org/fedora-minimal:$(FEDORA_VERSION))
DEV_RUNTIME_IMAGE ?= registry.fedoraproject.org/fedora-minimal:$(FEDORA_VERSION)

# ---------------------------------------------------------------------------
# Version information injected at build time.
# ---------------------------------------------------------------------------
VERSION_PKG := github.com/frobware/go-bpfman/version
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
GIT_BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
GIT_STATE ?= $(shell if git diff --quiet 2>/dev/null; then echo clean; else echo dirty; fi)
# Captured once so every reference returns the same timestamp. ?=
# would have been recursively-expanded and re-run `date` per use.
ifndef BUILD_DATE
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
endif
GIT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)

# ---------------------------------------------------------------------------
# Caller-tunable go build / go test / go ldflags.
# ---------------------------------------------------------------------------
# Extra flags appended to `go build` and `go test` recipes
# (e.g. EXTRA_GOFLAGS=-a to force a from-scratch rebuild).
EXTRA_GOFLAGS ?=

# Caller-supplied additional ldflags. Empty by default so local
# development still produces unstripped binaries with full symbol
# information for debugging; CI publish overrides this with -s -w
# to drop the symbol table and DWARF sections from shipped images.
EXTRA_GO_LDFLAGS ?=

# STAMP turns on version stamping (-X flags carrying git commit,
# branch, state, build date) on the bpfman and bpfman-shell
# binaries. Default off because the stamps invalidate Go's link
# cache on every invocation (timestamps and dirty-state change),
# which is wasted work for local development. CI sets STAMP=1 so
# released binaries report their provenance via `bpfman version`.
STAMP ?=

# ---------------------------------------------------------------------------
# Image attestation metadata, baked into the binary via -ldflags so
# `bpfman version` can print a ready-to-pipe `cosign verify` command
# for the image this binary was published from. All three default
# to empty: local `make build`, the host-build path via
# Dockerfile.bpfman.dev, and downstream Konflux/RHEL/UBI builds
# leave them unset, and the version printer omits the Attestation
# line entirely when any of them is empty. Only the CI image-build
# workflow (.github/workflows/image.yaml) populates them.
# ---------------------------------------------------------------------------
IMAGE_REF       ?=
SIGNER_IDENTITY ?=
OIDC_ISSUER     ?=

# ---------------------------------------------------------------------------
# Derived: GO_LDFLAGS composes STATIC, version stamping, image
# attestation, and EXTRA_GO_LDFLAGS. Must be defined after all of
# those so `:=` captures their final values.
#
# TEST_LDFLAGS is the subset relevant to test linking: only the
# static-link mode. Version stamps change on every invocation
# (timestamps, git state) and would force Go's test cache to
# relink every binary. Tests do not read the stamped values, so
# dropping them keeps `make test` fast.
# ---------------------------------------------------------------------------
TEST_LDFLAGS := $(strip \
    $(if $(STATIC),-linkmode=external -extldflags '-static') \
    $(EXTRA_GO_LDFLAGS))

GO_LDFLAGS := $(strip \
    $(TEST_LDFLAGS) \
    -X $(VERSION_PKG).gitCommit=$(GIT_COMMIT) \
    -X $(VERSION_PKG).gitBranch=$(GIT_BRANCH) \
    -X $(VERSION_PKG).gitState=$(GIT_STATE) \
    -X $(VERSION_PKG).buildDate=$(BUILD_DATE) \
    -X $(VERSION_PKG).version=$(GIT_VERSION) \
    $(if $(IMAGE_REF),-X $(VERSION_PKG).imageRef=$(IMAGE_REF)) \
    $(if $(SIGNER_IDENTITY),-X $(VERSION_PKG).signerIdentity=$(SIGNER_IDENTITY)) \
    $(if $(OIDC_ISSUER),-X $(VERSION_PKG).oidcIssuer=$(OIDC_ISSUER)))

# BIN_LDFLAGS is what the bpfman and bpfman-shell build recipes
# pass to `go build`. STAMP=1 selects the full GO_LDFLAGS (with
# version stamps); otherwise it falls back to the unstamped
# TEST_LDFLAGS. CI invokes ci-build with STAMP=1 so shipped
# binaries carry their provenance; local `make` defaults to
# unstamped binaries that hit the link cache on every rebuild.
BIN_LDFLAGS := $(if $(STAMP),$(GO_LDFLAGS),$(TEST_LDFLAGS))

# ---------------------------------------------------------------------------
# Build tags.
# ---------------------------------------------------------------------------
STATIC_TAGS := osusergo,netgo
EXTRA_TAGS ?=
# When the caller selects the mattn/go-sqlite3 driver via cgo_sqlite,
# also force sqlite_omit_load_extension. This compiles the embedded
# SQLite amalgamation with -DSQLITE_OMIT_LOAD_EXTENSION, dropping the
# sqlite3_load_extension() API and its unixDlOpen() wrapper -- the
# sole reason the static linker emits "Using 'dlopen' in statically
# linked applications requires at runtime the shared libraries from
# the glibc version used for linking" against the cgo_sqlite path. We
# never load runtime SQL extensions, so omitting them is pure
# subtraction. Apply only when cgo_sqlite is already in EXTRA_TAGS so
# default builds (modernc, no cgo) are unaffected.
ifneq (,$(filter cgo_sqlite,$(subst $(comma), ,$(EXTRA_TAGS))))
ifeq (,$(filter sqlite_omit_load_extension,$(subst $(comma), ,$(EXTRA_TAGS))))
override EXTRA_TAGS := $(EXTRA_TAGS),sqlite_omit_load_extension
endif
endif
# Tag sets consumed by each go build/test recipe. EXTRA_TAGS is
# appended to every set so callers can add a tag once (e.g.
# EXTRA_TAGS=cgo_sqlite) and have every build path pick it up.
BUILD_TAGS   := $(call comma-join,$(if $(STATIC),$(STATIC_TAGS)) $(EXTRA_TAGS))
TEST_TAGS    := $(BUILD_TAGS)
E2E_TAGS     := $(call comma-join,e2e $(if $(STATIC),$(STATIC_TAGS)) $(EXTRA_TAGS))
NSENTER_TAGS := $(call comma-join,nsenter $(EXTRA_TAGS))

# ---------------------------------------------------------------------------
# nsenter cross-architecture tests.
# ---------------------------------------------------------------------------
NSENTER_ARCHES ?= amd64 arm64 ppc64le s390x
NSENTER_TEST_BIN ?= $(BIN_DIR)/nsenter.test

# ---------------------------------------------------------------------------
# BPF build path.
#
# Build all BPF programs (dispatchers + e2e testdata) using the host
# toolchain: clang + libbpf headers + Linux UAPI headers. The Nix
# devShell provides these via clang-unwrapped + libbpf + linuxHeaders
# (see flake.nix); `hack/install-fedora-deps.sh` installs the
# equivalent Fedora RPMs (clang, llvm, libbpf-devel, kernel-headers,
# pkgconf-pkg-config). On stock Ubuntu CI runners, apt-get installs
# the equivalents (clang, llvm, libbpf-dev, linux-libc-dev,
# pkg-config). Konflux's Containerfile.bpfman.openshift compiles the
# BPF objects in its own first stage and does not invoke this rule.
# ---------------------------------------------------------------------------

# Shared compile setup. LIBBPF_CFLAGS comes from pkg-config so the
# include path follows the libbpf-devel package; BPF_CFLAGS is a
# caller knob (Ubuntu CI passes -I/usr/include/<DEB_HOST_MULTIARCH>
# so clang in -target bpfel mode finds asm/types.h under the
# multiarch include path).
#
# `=` (deferred) rather than `:=` (immediate) so pkg-config only
# fires when a recipe actually references LIBBPF_CFLAGS. The
# openshift Containerfile's go-builder stage runs `make bpfman-
# compile` against pre-built .bpf.o files (no BPF compile happens
# there), and that stage's image (ubi9/go-toolset) intentionally
# does not ship libbpf-devel; an immediate evaluation would emit a
# spurious "Package 'libbpf' not found" pkg-config warning.
LIBBPF_CFLAGS = $(shell pkg-config --cflags libbpf)
BPF_CFLAGS ?=

# clang -target bpfel produces architecture-independent BPF
# bytecode, but kernel UAPI headers it pulls in (asm/types.h and
# friends) are arch-specific. Define __TARGET_ARCH_<arch> to match
# the host so the right asm/ headers are used.
HOST_ARCH ?= $(shell uname -m)
ifeq ($(HOST_ARCH),x86_64)
    BPF_TARGET_ARCH := -D__TARGET_ARCH_x86
else ifeq ($(HOST_ARCH),i686)
    BPF_TARGET_ARCH := -D__TARGET_ARCH_x86
else ifeq ($(HOST_ARCH),aarch64)
    BPF_TARGET_ARCH := -D__TARGET_ARCH_arm64
else ifeq ($(HOST_ARCH),ppc64le)
    BPF_TARGET_ARCH := -D__TARGET_ARCH_powerpc
else ifeq ($(HOST_ARCH),powerpc64le)
    BPF_TARGET_ARCH := -D__TARGET_ARCH_powerpc
else ifeq ($(HOST_ARCH),s390x)
    BPF_TARGET_ARCH := -D__TARGET_ARCH_s390
else
    $(error unsupported HOST_ARCH=$(HOST_ARCH))
endif

# Dispatcher BPF: sources live in dispatcher/bpf/; the dispatcher
# Go package's go:embed directives read .bpf.o files at the package
# root (dispatcher/), so the compile rule below targets that path
# directly -- no intermediate copy needed. xdp_dispatcher_v1.bpf.c
# is kept as historical reference and is excluded from the build.
DISPATCHER_BPF_SOURCES := $(filter-out dispatcher/bpf/xdp_dispatcher_v1.bpf.c,$(wildcard dispatcher/bpf/*.bpf.c))
DISPATCHER_BPF_EMBEDS  := $(addprefix dispatcher/,$(notdir $(DISPATCHER_BPF_SOURCES:.bpf.c=.bpf.o)))
DISPATCHER_BPF_DEPS    := $(DISPATCHER_BPF_EMBEDS:.bpf.o=.bpf.d)

# E2E testdata BPF: sources and outputs all live in
# e2e/testdata/bpf/.
E2E_BPF_SOURCES := $(wildcard e2e/testdata/bpf/*.bpf.c)
E2E_BPF_OBJECTS := $(E2E_BPF_SOURCES:.bpf.c=.bpf.o)
E2E_BPF_DEPS    := $(E2E_BPF_SOURCES:.bpf.c=.bpf.d)

# platform/ebpf BPF: the package's discover_test.go embeds
# xdp_pass.bpf.o via go:embed, so the compile rule emits the
# object straight into platform/ebpf/ alongside the test source.
# Source still lives under e2e/testdata/bpf/ -- the BPF program
# is shared between the unit tests and the e2e suite, and
# duplicating the .bpf.c would create a divergence risk.
PLATFORM_EBPF_BPF_EMBEDS := platform/ebpf/xdp_pass.bpf.o
PLATFORM_EBPF_BPF_DEPS   := $(PLATFORM_EBPF_BPF_EMBEDS:.bpf.o=.bpf.d)

# ---------------------------------------------------------------------------
# Multi-arch buildx knobs.
#
# STATIC linkage is intentionally NOT a knob here. The Dockerfile
# hardcodes `make bpfman-compile STATIC=1` because the final stage is
# `scratch` and the two are coupled: a dynamically linked binary
# would crash immediately on a libc-less base. If you need a
# non-static binary (FIPS Go toolchains, dynamic-glibc bases), build
# on the host and package via Dockerfile.bpfman.dev instead.
#
# Multi-platform builds require a docker-container or remote buildx
# builder. CI workflows use docker/setup-buildx-action which
# provisions one automatically; locally, run `docker buildx create
# --driver docker-container --use` once.
# ---------------------------------------------------------------------------
PLATFORMS               ?=
PUSH                    ?=
BUILDX_EXTRA_ARGS       ?=
# Caller-supplied extra args passed last to the plain `docker build`
# targets (build-image-dev, build-image-csi-sanity, build-image-
# openshift). Positioned just before the build context so caller
# flags override any preceding hard-coded flags that buildx/docker
# treats as last-wins.
EXTRA_DOCKER_BUILD_ARGS ?=
# Selects which Dockerfile the buildx targets use. Defaults to the
# in-tree Dockerfile.bpfman; override to test an alternative
# dockerfile without editing the recipe.
BPFMAN_DOCKERFILE ?= Dockerfile.bpfman

# True (1) when $(OCI_BIN) is podman (either OCI_BIN=podman, or
# OCI_BIN=docker where docker is the podman compat shim). Buildah /
# podman does not honor the per-Dockerfile <dockerfile>.dockerignore
# convention that buildkit reads automatically, so when running
# under podman the build-image-dev recipe passes --ignorefile
# explicitly to point at the per-file dockerignore. Buildkit-
# backed `docker` does not need this and ignores --ignorefile
# (it is a buildah/podman flag), so detection is required.
#
# Safe to leave unguarded: $(OCI_BIN) is guaranteed non-empty (the
# "|| docker" fallback above), so the worst case is `docker
# --version 2>&1` inside a container where docker isn't installed,
# which produces "docker: command not found" and grep -c returns 0.
OCI_BIN_IS_PODMAN := $(shell $(OCI_BIN) --version 2>&1 | grep -ci podman)
# Optional path for buildx --metadata-file. When set, buildx writes
# the published index digest to this path after the push completes,
# and the cosign-sign target reads the digest from it. Empty by
# default; CI sets it to ${RUNNER_TEMP}/buildx-meta.json. Locally,
# any writable path works.
BUILDX_METADATA_FILE ?=

# Output-flag selection. Truth table:
#
#   PUSH=1                            -> --push
#   PLATFORMS contains a comma        -> no flag (cache-only)
#   otherwise                         -> --load
BUILDX_OUTPUT := $(if $(PUSH),--push,$(if $(findstring $(comma),$(PLATFORMS)),,--load))

# Provenance and SBOM attestations are only meaningful when pushing
# to a registry: the Docker image store strips OCI attestations on
# --load, and a cache-only build never produces an artifact to
# attest. Gating on PUSH avoids confusing buildx warnings.
BUILDX_ATTEST := $(if $(PUSH),--provenance=mode=max --sbom=true)

# ---------------------------------------------------------------------------
# OpenShift Containerfile build (local testing).
#
# Build via the same Containerfile that Konflux uses. The BPF
# builder stage defaults to UBI9 but can be overridden with Fedora
# for local testing without RHEL entitlements:
#
#   make build-image-openshift \
#     OPENSHIFT_BPF_BASE_IMAGE=fedora:43 \
#     OPENSHIFT_BPF_INSTALL_CMD="dnf install -y clang gcc kernel-headers libbpf-devel llvm make pkgconf-pkg-config && dnf clean all"
# ---------------------------------------------------------------------------
OPENSHIFT_CONTAINERFILE ?= Containerfile.bpfman.openshift
OPENSHIFT_BPF_BASE_IMAGE ?=
OPENSHIFT_BPF_INSTALL_CMD ?=

# ---------------------------------------------------------------------------
# Lint target lists.
#
# LINT_MAKE_TARGETS is the bundle that `make lint-make` runs under
# --warn-undefined-variables. Variables referenced only inside a
# recipe are deferred-expansion: a warning only fires once the recipe
# is selected for execution, so the bundle must exercise every recipe
# that pulls a caller-tunable variable (TEST, PARALLEL, PLATFORMS,
# EXTRA_*, etc.) for those references to get probed.
# ---------------------------------------------------------------------------
LINT_MAKE_TARGETS := \
	help \
	test test-e2e test-e2e-scripts \
	test-nsenter test-nsenter-amd64 test-nsenter-arm64 test-nsenter-cross \
	bpfman-compile \
	build-image build-image-amd64 build-image-dev \
	build-image-csi-sanity build-image-openshift \
	ci-build ci-check-fmt ci-check-vendor ci-check-vet ci-image ci-lint ci-test ci-test-e2e ci-test-e2e-scripts \
	cosign-sign coverage clean

# Lint every Dockerfile / Containerfile with hadolint. The existing
# `# hadolint ignore=...` pragmas in the repo are already set up
# for this tool; adding the target wires it into CI.
LINT_DOCKERFILES := \
	Dockerfile.bpfman.dev \
	Dockerfile.bpfman \
	Dockerfile.ci \
	Dockerfile.csi-sanity \
	Containerfile.bpfman.openshift


# ============================================================================
# Targets
# ============================================================================

# ---------------------------------------------------------------------------
# Meta: default target, help, clean, version prints, bin directory.
# ---------------------------------------------------------------------------
all: bpfman-build bpfman-shell-build

# Alias so 'make build-all' works as advertised in 'make help'.
# 'all' stays the canonical default-target name; 'build-all' is
# the spelling the help text and tab-completion expose.
build-all: all

help:
	@echo "Build:"
	@echo "  build-all                   Build all binaries"
	@echo "  clean                       Remove all build artifacts"
	@echo "  clean-mrproper              Like 'clean', plus wipe Go's shared build/test/fuzz caches (~/.cache/go-build); affects all Go projects on this machine"
	@echo ""
	@echo "Testing:"
	@echo "  test                        Run all tests"
	@echo "  test-e2e                    Run e2e tests (requires root)"
	@echo "  test-e2e-grpc               Run the parallel gRPC e2e test against a real bpfman serve daemon (requires root)"
	@echo "  test-e2e-scripts            Run REPL e2e scripts under e2e/scripts/ and e2e/new/ (requires root)"
	@echo "  test-examples               Run REPL scripts under examples/ (requires root)"
	@echo "  test-nsenter                Run nsenter tests (native amd64)"
	@echo "  test-nsenter-cross          Run nsenter tests on amd64/arm64/ppc64le/s390x"
	@echo "  test-nsenter-{arch}         Run nsenter tests for a single architecture"
	@echo "  lint                        Run golangci-lint"
	@echo "  coverage                    Generate coverage profile and show total"
	@echo "  coverage-func               Show coverage by function"
	@echo "  coverage-html               Generate HTML coverage report"
	@echo "  coverage-open               Generate and open HTML coverage report"
	@echo "  clean-coverage              Remove coverage artifacts"
	@echo ""
	@echo "Local CI reproducer (Dockerfile.ci):"
	@echo "  ci                          Run every ci-* target"
	@echo "  ci-build                    Compile bpfman binary inside the CI container"
	@echo "  ci-check-fmt                Verify Go formatting is tidy (matches CI check-fmt)"
	@echo "  ci-check-goimports          Verify Go imports are tidy (matches CI check-goimports)"
	@echo "  ci-check-vendor             Verify go.mod and vendor are tidy (matches CI check-vendor)"
	@echo "  ci-check-vet                Run go vet over every build-tag combo (matches CI check-vet)"
	@echo "  ci-image                    Build the CI base image (loaded as bpfman-ci)"
	@echo "  ci-lint                     Run \`make lint\` inside the CI container"
	@echo "  ci-test                     Run unit tests inside the CI container"
	@echo "  ci-test-e2e                 Extract e2e test bundle and run it on the host (sudo)"
	@echo "  ci-test-e2e-scripts         Extract bundle to source tree and run REPL scripts (sudo)"
	@echo ""
	@echo "bpfman (with integrated CSI):"
	@echo "  bpfman-build                Build bpfman binary"
	@echo "  bpfman-compile              Compile bpfman (no fmt/vet/dispatchers)"
	@echo "  clean-bpfman                Remove generated files and binary"
	@echo "  bpfman-proto                Generate protobuf/gRPC stubs"
	@echo "  bpfman-test-grpc            Run gRPC integration tests"
	@echo ""
	@echo "Container images:"
	@echo "  build-image                 Cross-compile current-arch image via Fedora Dockerfile (canonical pipeline)"
	@echo "  build-image-{arch}          Cross-compile single-arch image (arch in amd64/arm64/ppc64le/s390x)"
	@echo "  build-image-csi-sanity      Build csi-sanity container image"
	@echo "  build-image-dev             Build current-arch image from host-built binary (fast dev iteration)"
	@echo "  build-image-nix             Pure-Nix OCI image (no Docker daemon at build time; debug toolkit baked in)"
	@echo "  build-image-openshift       Build via OpenShift Containerfile (local test)"
	@echo "  cosign-sign                 Sign a published image (requires BUILDX_METADATA_FILE)"
	@echo ""
	@echo "Documentation:"
	@echo "  doc                         Start pkgsite documentation server"
	@echo "  doc-text                    Print API documentation to stdout"
	@echo ""
	@echo "BPF:"
	@echo "  clean-bpf                   Remove BPF build artefacts"
	@echo "  (no bpf-build target -- consumers depend directly on .bpf.o outputs)"
	@echo ""
	@echo "SQLite driver:"
	@echo "  The default SQLite driver is modernc.org/sqlite (pure Go)."
	@echo "  To use mattn/go-sqlite3 (CGO) instead, pass -tags cgo_sqlite:"
	@echo "    go build -tags cgo_sqlite ./..."
	@echo "    go test -tags cgo_sqlite ./..."

print-go-version:
	@echo $(GO_VERSION)

print-fedora-version:
	@echo $(FEDORA_VERSION)

print-golangci-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

clean: clean-bpfman clean-bpfman-shell clean-bpf clean-coverage
	$(RM) -r $(BIN_DIR) $(CI_E2E_BUNDLE)

# Nuclear option, modeled on `make mrproper` in the kernel tree:
# wipe local build artifacts AND Go's shared caches under
# ~/.cache/go-build. Useful when chasing cache-coherence bugs whose
# inputs aren't in cmd/go's action key (e.g. GO_EXTLINK_ENABLED — see
# flake.nix). Affects every Go project sharing this user's cache, not
# just this checkout. The module cache is intentionally NOT wiped:
# `go clean -modcache` forces a full re-download on the next build.
clean-mrproper: clean
	go clean -cache -testcache -fuzzcache

# Ensure bin directory exists
$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

# ---------------------------------------------------------------------------
# Lint.
# ---------------------------------------------------------------------------
# Uber lint target: run every language-specific linter in turn.
# Keep each sub-target independently runnable so contributors can
# iterate on one layer at a time.
lint: lint-go lint-make lint-hack lint-dockerfile

lint-go: $(DISPATCHER_BPF_EMBEDS) $(PLATFORM_EBPF_BPF_EMBEDS) $(BIN_DIR)/golangci-lint
	$(BIN_DIR)/golangci-lint run

$(BIN_DIR)/golangci-lint: | $(BIN_DIR)
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(abspath $(BIN_DIR)) $(GOLANGCI_LINT_VERSION)

# Lint the Makefile itself.
#
# Layer 1: checkmake (reads checkmake.ini for rule thresholds).
#
# Layer 2: GNU Make's `--warn-undefined-variables` in dry-run mode
# against a bundle of representative targets (LINT_MAKE_TARGETS).
# Any warning is escalated to an error.
lint-make:
	checkmake --config=checkmake.ini Makefile
	@echo "Probing --warn-undefined-variables across representative targets..."
	@if $(MAKE) --warn-undefined-variables --no-print-directory -n $(LINT_MAKE_TARGETS) 2>&1 \
	    | grep -E '^Makefile:.*warning:'; then \
	    echo "FAIL: --warn-undefined-variables reported issues"; \
	    exit 1; \
	fi
	@echo "--warn-undefined-variables: clean"

# Lint every shell script under hack/ recursively so subdirectories
# (hack/openshift/, etc.) are covered. -x lets shellcheck follow
# source-statements to other files in the tree.
lint-hack:
	find hack -type f -name '*.sh' -exec shellcheck -x {} +

lint-dockerfile:
	hadolint $(LINT_DOCKERFILES)

# ---------------------------------------------------------------------------
# Tests.
# ---------------------------------------------------------------------------
# platform/ebpf unit tests embed xdp_pass.bpf.o via go:embed, so
# the embed object must exist at `go test -c` time. The dispatcher
# embeds are needed because dispatcher tests likewise go:embed
# their .bpf.o files. Unit tests no longer reach into
# e2e/testdata/bpf/ at runtime.
test: $(DISPATCHER_BPF_EMBEDS) $(PLATFORM_EBPF_BPF_EMBEDS)
	$(strip go test $(if $(RACE),-race,) $(EXTRA_GOFLAGS) $(if $(TEST_TAGS),-tags '$(TEST_TAGS)') $(if $(STATIC),-ldflags "$(TEST_LDFLAGS)") -v $(if $(PARALLEL),-parallel $(PARALLEL)) ./...)

# nsenter cross-architecture tests
#
# Proves the nsenter package's C constructor and nsexec code compile,
# link, and run on each target architecture. Uses cross-compilation
# GCC and QEMU user-mode emulation for foreign architectures.
#
# The CC is auto-detected: Nix-style triples are tried first
# (<prefix>-unknown-linux-gnu-gcc), then distro-style
# (<prefix>-linux-gnu-gcc). QEMU adds -L <sysroot> automatically
# when a distro sysroot directory exists (/usr/<prefix>-linux-gnu).
#
# Usage:
#   make test-nsenter                 # native amd64 only
#   make test-nsenter-arm64           # single foreign architecture
#   make test-nsenter-cross           # all architectures
test-nsenter test-nsenter-amd64: | $(BIN_DIR)
	@echo "=== nsenter: amd64 ==="
	$(strip CGO_ENABLED=1 go test -c $(EXTRA_GOFLAGS) $(if $(NSENTER_TAGS),-tags=$(NSENTER_TAGS)) -o $(NSENTER_TEST_BIN) ./ns/nsenter/)
	file $(NSENTER_TEST_BIN)
	sudo $(NSENTER_TEST_BIN) -test.v

test-nsenter-arm64 test-nsenter-ppc64le test-nsenter-s390x:
	NSENTER_TEST_BIN=$(NSENTER_TEST_BIN) NSENTER_TAGS=$(NSENTER_TAGS) \
		hack/test-nsenter-cross.sh $(@:test-nsenter-%=%)

test-nsenter-cross: $(addprefix test-nsenter-,$(NSENTER_ARCHES))

# Phony so the recipe always runs; go's own build cache decides
# whether anything actually rebuilds. Mirrors the bpfman-compile
# pattern -- Make's mtime tracking would otherwise lie when the
# inputs are .go files we haven't enumerated as prereqs.
$(BIN_DIR)/e2e.test: $(DISPATCHER_BPF_EMBEDS) $(E2E_BPF_OBJECTS) | $(BIN_DIR)
	$(strip go test -c $(if $(RACE),-race,) $(EXTRA_GOFLAGS) $(if $(E2E_TAGS),-tags=$(E2E_TAGS)) $(if $(STATIC),-ldflags "$(TEST_LDFLAGS)") -o $(BIN_DIR)/e2e.test ./e2e)

# STRESS_COUNT is honoured here too: `-test.count=$(STRESS_COUNT)`
# is harmless at the default 1 and turns the same recipe into a
# stress run when bumped (CI pins it to 5 so every PR gets a small
# count loop on top of the deterministic gate).
test-e2e: $(BIN_DIR)/e2e.test
	sudo $(if $(ISOLATED_RUNTIME),BPFMAN_E2E_ISOLATED_RUNTIME=$(ISOLATED_RUNTIME)) $(BIN_DIR)/e2e.test -test.v -test.failfast -test.count=$(STRESS_COUNT) $(if $(PARALLEL),-test.parallel $(PARALLEL)) $(if $(TEST),-test.run $(TEST))

# Parallel gRPC e2e: stands up a real `bpfman serve` subprocess and
# fans goroutines through load/get/attach/detach/unload over the
# socket. The test resolves bin/bpfman via the source tree, so the
# daemon binary must be built; bpfman-compile is a hard prereq.
# BPFMAN_GRPC_PARALLEL_N and BPFMAN_GRPC_PARALLEL_ITERS are the
# concurrency knobs; BPFMAN_LOG controls the daemon-side log spec
# (e.g. info,lock=debug,store=debug). All three are forwarded into
# the sudo'd test process.
#
# Test output is split: the full daemon + test transcript goes to
# $(GRPC_TEST_LOG) (override on the command line if you need a
# different path), and only the test framework's PASS/FAIL lines
# and any SQLite BUSY signals are streamed to the terminal.
# That keeps the terminal readable when the daemon is running with
# the verbose component logging the investigation paths need, while
# preserving the full trace for post-mortem analysis.
#
# Exit code preservation: `set -o pipefail` in a bash sub-shell
# ensures the test binary's non-zero exit propagates through the
# `tee | awk` pipeline. awk-not-grep is deliberate: grep returns 1
# when nothing matches, which would mask a passing test as a
# failure.
GRPC_TEST_LOG ?= /tmp/bpfman-test-e2e-grpc.log

$(BIN_DIR)/e2e-grpc.test: $(DISPATCHER_BPF_EMBEDS) $(E2E_BPF_OBJECTS) | $(BIN_DIR)
	$(strip go test -c $(if $(RACE),-race,) $(EXTRA_GOFLAGS) $(if $(E2E_TAGS),-tags=$(E2E_TAGS)) $(if $(STATIC),-ldflags "$(TEST_LDFLAGS)") -o $(BIN_DIR)/e2e-grpc.test ./e2e/grpc)

test-e2e-grpc: $(BIN_DIR)/e2e-grpc.test bpfman-compile
	@echo "Full log: $(GRPC_TEST_LOG)"
	@bash -c 'set -o pipefail; sudo $(if $(BPFMAN_GRPC_PARALLEL_N),BPFMAN_GRPC_PARALLEL_N=$(BPFMAN_GRPC_PARALLEL_N)) $(if $(BPFMAN_GRPC_PARALLEL_ITERS),BPFMAN_GRPC_PARALLEL_ITERS=$(BPFMAN_GRPC_PARALLEL_ITERS)) $(if $(BPFMAN_LOG),BPFMAN_LOG=$(BPFMAN_LOG)) $(BIN_DIR)/e2e-grpc.test -test.v -test.failfast -test.count=$(STRESS_COUNT) $(if $(TEST),-test.run $(TEST)) 2>&1 | tee $(GRPC_TEST_LOG) | awk "/--- PASS:|--- FAIL:|^PASS\$$|^FAIL\$$|SQLITE_BUSY|tx begin failed/"'

# Run every REPL script under e2e/scripts/ and e2e/new/ against
# the built bpfman binary. Each script executes from e2e/ so
# testdata paths match the Go e2e tests. The target runs them
# sequentially, reports failures as it goes, and exits non-zero
# at the end if any script failed. Pass TEST=<name> to restrict
# to scripts whose filename contains <name>.
# Split into build + run so CI can extract pre-built artefacts
# from a hermetic container build (Dockerfile.ci's e2e-export
# stage) and invoke `run-e2e-scripts` directly on the runner
# without re-triggering the build deps. Local invocations of
# `test-e2e-scripts` still build first.
build-e2e-scripts: bpfman-compile bpfman-shell-compile $(BIN_DIR)/e2e.test

run-e2e-scripts:
	@echo "Running REPL e2e scripts (requires root)..."
	BIN_DIR=$(BIN_DIR) hack/test-e2e-scripts.sh $(TEST)

test-e2e-scripts: build-e2e-scripts run-e2e-scripts

# Run every REPL script under examples/ against the built bpfman
# binary. The examples are load/attach/detach/unload
# walk-throughs; running them in CI catches drift between the
# shipped examples and the actual CLI surface. Pass TEST=<name> to
# restrict to scripts whose filename contains <name>.
test-examples: bpfman-compile bpfman-shell-compile $(BIN_DIR)/e2e.test
	@echo "Running REPL example scripts (requires root)..."
	BIN_DIR=$(BIN_DIR) hack/test-examples.sh $(TEST)

# ---------------------------------------------------------------------------
# Coverage.
# ---------------------------------------------------------------------------
coverage:
	@mkdir -p $(COVERAGE_DIR)
	@$(strip go test $(EXTRA_GOFLAGS) -coverprofile=$(COVERAGE_PROFILE) ./...) 2>&1 | grep -v "no test files" | grep -v "no such tool" | grep -v "^#"
	@echo "Coverage profile written to $(COVERAGE_PROFILE)"
	@go tool cover -func=$(COVERAGE_PROFILE) 2>/dev/null | grep total

coverage-html: coverage
	go tool cover -html=$(COVERAGE_PROFILE) -o $(COVERAGE_HTML)
	@echo "Coverage report written to $(COVERAGE_HTML)"

coverage-func: coverage
	go tool cover -func=$(COVERAGE_PROFILE)

coverage-open: coverage-html
	xdg-open $(COVERAGE_HTML) 2>/dev/null || open $(COVERAGE_HTML) 2>/dev/null || echo "Open $(COVERAGE_HTML) in your browser"

clean-coverage:
	$(RM) -r $(COVERAGE_DIR)

# ---------------------------------------------------------------------------
# Documentation.
# ---------------------------------------------------------------------------
doc:
	@echo "Starting pkgsite documentation server..."
	@echo "Open http://localhost:$(DOC_PORT)/github.com/frobware/go-bpfman"
	@echo "Press Ctrl+C to stop"
	@go run golang.org/x/pkgsite/cmd/pkgsite@latest -http=localhost:$(DOC_PORT) .

doc-text:
	@echo "=== Public API ===" && echo
	@for pkg in ./bpfman ./client ./csi; do \
		echo "--- $$pkg ---" && go doc -all $$pkg 2>/dev/null && echo; \
	done

# ---------------------------------------------------------------------------
# bpfman build.
#
# Note: bpfman-proto is not a dependency here since pb files are committed.
# Run 'make bpfman-proto' explicitly after modifying proto/bpfman.proto.
# CGO is required for the ns/nsenter package which uses a C constructor to call
# setns() before Go runtime starts (needed for uprobe container attachment).
# ---------------------------------------------------------------------------
bpfman-build: bpfman-fmt bpfman-compile

# Format every .go file in the tree. `go fmt ./...` skips files that
# don't compile under the default build tags (e.g. anything behind
# //go:build e2e), so we'd silently miss formatting drift in e2e/.
# gofmt invoked directly on the file list ignores build tags and
# formats every source file, matching what ci-check-fmt expects.
bpfman-fmt:
	@find . -type f -name '*.go' -not -path './vendor/*' -print0 | xargs -0 gofmt -w

# Apply gofmt + goimports to every .go file via golangci-lint's `fmt`
# subcommand, which honours the formatters block in .golangci.yml --
# notably the goimports local-prefixes pin. Goes via golangci-lint
# rather than a standalone goimports binary because the static dev
# shell's glibc.static link path makes a freshly-installed
# goimports segfault at runtime (NSS dlopen). golangci-lint is
# already pinned for `make lint` so reusing it costs nothing.
bpfman-goimports: $(BIN_DIR)/golangci-lint
	$(BIN_DIR)/golangci-lint fmt

# Run go vet over every build-tag combination present in the tree.
# `go vet ./...` honours the active tag set; a single pass under the
# default tags would skip files behind //go:build e2e (entire e2e/
# package), //go:build nsenter (CGO-namespaced helper), and the
# cgo_sqlite alternate driver path. Cover them in three passes:
#   - Default pass: most code plus the modernc.org/sqlite branch
#     (!cgo_sqlite).
#   - e2e+nsenter pass: adds the build-tagged files; supersets the
#     default pass for everything not gated by cgo_sqlite.
#   - cgo_sqlite pass: covers the mattn/go-sqlite3 alternate, which
#     is mutually exclusive with !cgo_sqlite.
bpfman-vet: $(DISPATCHER_BPF_EMBEDS) $(PLATFORM_EBPF_BPF_EMBEDS) $(E2E_BPF_OBJECTS)
	go vet ./...
	go vet -tags 'e2e,nsenter' ./...
	go vet -tags 'cgo_sqlite,e2e,nsenter' ./...

# Compile bpfman. Depends on the dispatcher BPF embeds because
# the dispatcher Go package's go:embed directives need them at
# compile time. Make's pattern rules build them on demand if
# missing or out of date.
bpfman-compile: $(DISPATCHER_BPF_EMBEDS) | $(BIN_DIR)
	$(strip CGO_ENABLED=1 go build $(if $(RACE),-race,) $(EXTRA_GOFLAGS) $(if $(BUILD_TAGS),-tags '$(BUILD_TAGS)') -ldflags "$(BIN_LDFLAGS)" -o $(BIN_DIR)/bpfman ./cmd/bpfman)

clean-bpfman:
	$(RM) $(BIN_DIR)/bpfman

# bpfman-shell is the development / test / ops companion to bpfman.
# It hosts the REPL, the DSL script runner, and (in time) the test
# scaffolding subcommands. Production deployments must ship only
# bin/bpfman; bin/bpfman-shell is intended for dev and CI.
bpfman-shell-build: bpfman-fmt bpfman-shell-compile

bpfman-shell-compile: | $(BIN_DIR)
	$(strip CGO_ENABLED=1 go build $(if $(RACE),-race,) $(EXTRA_GOFLAGS) $(if $(BUILD_TAGS),-tags '$(BUILD_TAGS)') -ldflags "$(BIN_LDFLAGS)" -o $(BIN_DIR)/bpfman-shell ./cmd/bpfman-shell)

clean-bpfman-shell:
	$(RM) $(BIN_DIR)/bpfman-shell

# ---------------------------------------------------------------------------
# Proto generation for bpfman gRPC API.
# ---------------------------------------------------------------------------
bpfman-proto: $(BPFMAN_PB_DIR)/bpfman.pb.go $(BPFMAN_PB_DIR)/bpfman_grpc.pb.go

# protoc discovers --go_out / --go-grpc_out plugins on PATH, so the
# generated-stub rule prepends $(BIN_DIR) before invoking protoc.
# The protoc-gen-* binaries are order-only prerequisites (after `|`)
# so a fresh checkout that lacks them builds the plugins once, but
# their mtime does not invalidate the committed .pb.go files.
$(BPFMAN_PB_DIR)/bpfman.pb.go $(BPFMAN_PB_DIR)/bpfman_grpc.pb.go: \
		$(BPFMAN_PROTO_DIR)/bpfman.proto \
		| $(BIN_DIR)/protoc-gen-go $(BIN_DIR)/protoc-gen-go-grpc
	mkdir -p $(BPFMAN_PB_DIR)
	PATH="$(abspath $(BIN_DIR)):$$PATH" \
	protoc --go_out=$(BPFMAN_PB_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(BPFMAN_PB_DIR) --go-grpc_opt=paths=source_relative \
		--proto_path=$(BPFMAN_PROTO_DIR) \
		$<

# Vendor protoc plugins into $(BIN_DIR) so the Fedora-only build
# path does not need them on $PATH separately. Mirrors the
# golangci-lint pattern above. Versions are pinned via the
# PROTOC_GEN_*_VERSION variables; bump them and flake.nix's
# protoc-gen-go / protoc-gen-go-grpc pins together.
$(BIN_DIR)/protoc-gen-go: | $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) go install \
		google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(BIN_DIR)/protoc-gen-go-grpc: | $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) go install \
		google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

# ---------------------------------------------------------------------------
# BPF build rules.
#
# No `bpf-build` umbrella target: consumers depend directly on
# the actual .bpf.o outputs they need ($(DISPATCHER_BPF_EMBEDS)
# for the production binary, $(E2E_BPF_OBJECTS) for tests that
# exercise e2e BPF programs). Make's dependency graph handles
# incremental rebuilds against the real outputs without needing
# a phony intermediary.
# ---------------------------------------------------------------------------
dispatcher/%.bpf.o: dispatcher/bpf/%.bpf.c Makefile
	$(call quiet_cmd,CLANG-BPF,$@)
	$(Q)clang $(LIBBPF_CFLAGS) $(BPF_CFLAGS) -g -O2 -target bpfel -c $(BPF_TARGET_ARCH) \
		-MD -MP -MF$(@:.bpf.o=.bpf.d) $< -o $@

e2e/testdata/bpf/%.bpf.o: e2e/testdata/bpf/%.bpf.c Makefile
	$(call quiet_cmd,CLANG-BPF,$@)
	$(Q)clang $(LIBBPF_CFLAGS) $(BPF_CFLAGS) -g -O2 -target bpfel -c $(BPF_TARGET_ARCH) \
		-MD -MP -MF$(@:.bpf.o=.bpf.d) $< -o $@

# platform/ebpf consumes the same .bpf.c sources as the e2e tests
# but needs the compiled object next to the Go test files for
# go:embed. Mirrors the dispatcher pattern: emit straight into the
# consuming package's directory, no intermediate cp.
platform/ebpf/%.bpf.o: e2e/testdata/bpf/%.bpf.c Makefile
	$(call quiet_cmd,CLANG-BPF,$@)
	$(Q)clang $(LIBBPF_CFLAGS) $(BPF_CFLAGS) -g -O2 -target bpfel -c $(BPF_TARGET_ARCH) \
		-MD -MP -MF$(@:.bpf.o=.bpf.d) $< -o $@

clean-bpf:
	$(RM) $(DISPATCHER_BPF_EMBEDS) $(DISPATCHER_BPF_DEPS) \
	      $(E2E_BPF_OBJECTS) $(E2E_BPF_DEPS) \
	      $(PLATFORM_EBPF_BPF_EMBEDS) $(PLATFORM_EBPF_BPF_DEPS)

-include $(DISPATCHER_BPF_DEPS) $(E2E_BPF_DEPS) $(PLATFORM_EBPF_BPF_DEPS)

# ---------------------------------------------------------------------------
# Docker image builds.
# ---------------------------------------------------------------------------

# Build bpfman image from the host-built binary. Intended for local
# development and operator integration testing.
build-image-dev: bpfman-build
	$(OCI_BIN) build \
		$(if $(filter-out 0,$(OCI_BIN_IS_PODMAN)),--ignorefile=Dockerfile.bpfman.dev.dockerignore) \
		-t $(BPFMAN_IMG) \
		--build-arg RUNTIME_IMAGE=$(DEV_RUNTIME_IMAGE) \
		-f Dockerfile.bpfman.dev \
		$(EXTRA_DOCKER_BUILD_ARGS) .

# Canonical bpfman image build via buildx and the Fedora multiarch
# Dockerfile. The same recipe drives dev and CI; mode is selected
# by the variable knobs below.
#
# Modes:
#
#   make build-image
#       Default: current arch only, loaded into the local Docker
#       store. The cross-compile happens inside the container, so no
#       host toolchain is required (contrast with build-image-dev,
#       which packages a host-built binary).
#
#   make build-image-{amd64,arm64,ppc64le,s390x}
#       Per-arch presets that pin PLATFORMS to a single foreign arch
#       and --load. Useful when you want to run a foreign-arch image
#       under host binfmt + QEMU.
#
#   make build-image PLATFORMS=linux/amd64,linux/arm64,linux/ppc64le,linux/s390x
#       Multi-arch, cache-only build (no output). The local Docker
#       store cannot hold a multi-arch manifest, so the manifest
#       stays in the BuildKit cache. Useful as a "does it all
#       compile?" sanity check.
#
#   make build-image \
#       PLATFORMS=linux/amd64,linux/arm64,linux/ppc64le,linux/s390x \
#       PUSH=1 \
#       BPFMAN_IMG=<registry/repo:tag>
#       CI publish path: pushes a multi-arch manifest to the
#       registry, with SLSA build provenance (mode=max) and SBOM
#       attestations attached per platform.

# Pure-Nix OCI image, byte-reproducible and built without invoking
# a Docker daemon. Pulls the layered tarball that nix produces and
# `docker load`s it in one shot; --no-link keeps the workspace free
# of a stray result symlink that could collide with `nix build .`.
# See nix/image.nix for what is in the image and why.
build-image-nix:
	$(OCI_BIN) load < $$(nix build .#bpfman-image --print-out-paths --no-link)

build-image:
	$(OCI_BIN) buildx build \
		$(if $(PLATFORMS),--platform $(PLATFORMS)) \
		$(BUILDX_OUTPUT) \
		$(BUILDX_ATTEST) \
		$(if $(BUILDX_METADATA_FILE),--metadata-file=$(BUILDX_METADATA_FILE)) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GIT_BRANCH=$(GIT_BRANCH) \
		--build-arg GIT_VERSION=$(GIT_VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		$(if $(STATIC),--build-arg STATIC=1) \
		--build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		--build-arg EXTRA_GOFLAGS="$(EXTRA_GOFLAGS)" \
		--build-arg EXTRA_GO_LDFLAGS="$(EXTRA_GO_LDFLAGS)" \
		--build-arg IMAGE_REF="$(IMAGE_REF)" \
		--build-arg SIGNER_IDENTITY="$(SIGNER_IDENTITY)" \
		--build-arg OIDC_ISSUER="$(OIDC_ISSUER)" \
		-f $(BPFMAN_DOCKERFILE) \
		-t $(BPFMAN_IMG) \
		$(BUILDX_EXTRA_ARGS) .

# Per-arch presets pinning PLATFORMS to a single foreign arch.
# Each invocation builds one platform and --loads it into the local
# Docker store under the default $(BPFMAN_IMG) ref (e.g.
# quay.io/bpfman/bpfman:latest). The arch is implicit in the make
# target chosen, so the pullspec does not encode it; each
# invocation overwrites the previous one. To keep multiple arches
# loaded simultaneously, pass BPFMAN_IMG explicitly with distinct
# tags.
#
#   make build-image-amd64
#   make build-image-arm64
#   make build-image-ppc64le
#   make build-image-s390x
#
# The CI publish path uses `build-image` directly with a comma-
# separated PLATFORMS list; these presets are purely local-dev
# shortcuts.
build-image-amd64 build-image-arm64 build-image-ppc64le build-image-s390x: build-image-%:
	$(MAKE) build-image PLATFORMS=linux/$*

# Sign a published multi-arch image with cosign, anchored to the
# immutable index digest rather than the mutable tag.
#
# This target reads the digest from the buildx metadata file
# produced by the previous build-image run, so the same Make
# recipe serves both CI and local testing.
#
# CI usage (keyless via GitHub Actions OIDC):
#
#   make build-image \
#     PUSH=1 \
#     BPFMAN_IMG=<registry/repo:tag> \
#     BUILDX_METADATA_FILE=$${RUNNER_TEMP}/buildx-meta.json \
#     ...
#   make cosign-sign \
#     BPFMAN_IMG=<registry/repo:tag> \
#     BUILDX_METADATA_FILE=$${RUNNER_TEMP}/buildx-meta.json
#
# Local usage (interactive OAuth signing identity):
#
#   nix shell nixpkgs#cosign      # cosign is not in the dev profile
#
#   make build-image \
#     PLATFORMS=linux/amd64 \
#     PUSH=1 \
#     BPFMAN_IMG=ttl.sh/frobware/go-bpfman-test:latest \
#     BUILDX_METADATA_FILE=/tmp/buildx-meta.json
#
#   make cosign-sign \
#     BPFMAN_IMG=ttl.sh/frobware/go-bpfman-test:latest \
#     BUILDX_METADATA_FILE=/tmp/buildx-meta.json
#
# The local invocation triggers an interactive browser OAuth flow;
# the resulting Rekor record is tied to the user's personal
# identity (Google, GitHub, etc.) rather than to a workflow OIDC
# token. The mechanics are otherwise identical to CI.
cosign-sign:
	@command -v cosign >/dev/null 2>&1 || { \
		echo "error: cosign is not installed; try 'nix shell nixpkgs#cosign'" >&2; \
		exit 1; \
	}
	@command -v jq >/dev/null 2>&1 || { \
		echo "error: jq is not installed" >&2; \
		exit 1; \
	}
	@if [ -z "$(BUILDX_METADATA_FILE)" ]; then \
		echo "error: BUILDX_METADATA_FILE must be set" >&2; \
		echo "       (re-run build-image with the same value first)" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(BUILDX_METADATA_FILE)" ]; then \
		echo "error: $(BUILDX_METADATA_FILE) does not exist" >&2; \
		echo "       (run build-image first to produce it)" >&2; \
		exit 1; \
	fi
	@digest=$$(jq -r '."containerimage.digest" // empty' "$(BUILDX_METADATA_FILE)"); \
	if [ -z "$$digest" ]; then \
		echo "error: containerimage.digest missing from $(BUILDX_METADATA_FILE)" >&2; \
		cat "$(BUILDX_METADATA_FILE)" >&2; \
		exit 1; \
	fi; \
	echo "Signing $(BPFMAN_IMG)@$$digest"; \
	cosign sign -y "$(BPFMAN_IMG)@$$digest"

# CSI conformance testing
build-image-csi-sanity:
	$(OCI_BIN) build -t $(CSI_SANITY_IMG) -f Dockerfile.csi-sanity $(EXTRA_DOCKER_BUILD_ARGS) .

build-image-openshift:
	$(OCI_BIN) build \
		-f $(OPENSHIFT_CONTAINERFILE) \
		$(if $(OPENSHIFT_BPF_BASE_IMAGE),--build-arg BPF_BASE_IMAGE=$(OPENSHIFT_BPF_BASE_IMAGE)) \
		$(if $(OPENSHIFT_BPF_INSTALL_CMD),--build-arg BPF_INSTALL_CMD="$(OPENSHIFT_BPF_INSTALL_CMD)") \
		--build-arg BUILD_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_BRANCH=$(GIT_BRANCH) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg BUILD_VERSION=$(GIT_VERSION) \
		-t $(BPFMAN_IMG) \
		$(EXTRA_DOCKER_BUILD_ARGS) .

# ---------------------------------------------------------------------------
# Local CI reproducer.
#
# `make ci` runs every pipeline the GH workflows run -- vendor /
# format checks, the bpfman binary build, the lint umbrella, the
# unit tests, and the two e2e jobs (Go binary + REPL scripts).
# The CI workflow YAML invokes the same `make ci-*` targets, so
# `make ci` locally is a faithful reproduction of what runs in
# CI; if it passes here, it passes there (modulo runner-specific
# behaviour like NOPASSWD sudo or GHA cache backend).
#
# See Dockerfile.ci for the build environment those targets run
# inside.
# ---------------------------------------------------------------------------

# Build the `base` stage of Dockerfile.ci as a tagged image, ready
# for `docker run` invocations against a mounted source tree. The
# `--load` is required for `docker run` to find the image in the
# local store.
ci-image:
	$(OCI_BIN) buildx build --target=base -t $(CI_IMAGE) -f $(CI_DOCKERFILE) --load $(CI_BUILDX_CACHE) .

# Reproduce the workflow's build job locally. Verifies that the
# bpfman binary itself compiles -- separable from `ci-test`
# because `go test ./...` does not exercise the cmd/bpfman link
# path. STATIC=1 is intentionally omitted: static linking is a
# property we need when crossing the container/runner boundary
# (i.e. when we extract the artefact). Here the binary is
# verified-then-discarded inside the container, so the dynamic
# build is sufficient and avoids the noisy glibc-static
# warnings. The static-link path stays covered by the e2e jobs
# (which do extract) and by image.yaml (which ships).
#
# Go's build and module caches are persisted in named docker
# volumes so subsequent runs benefit from incremental compile.
# CI runners are ephemeral and don't benefit from this directly
# (each runner starts with empty volumes); the volumes are
# specifically for local iteration speed.
#
# `make clean-bpf` runs first because $(CI_RUN) bind-mounts the
# source tree (-v $(CURDIR):/src), and bind mounts bypass the
# dockerignore filter -- without it, host-built dispatcher/*.bpf.o
# would be visible inside the container, and Make would skip the
# in-container BPF compile based on host mtimes. The compile is
# cheap (~1-2s) and the Go cache volumes carry the rest of the
# incremental story. ci-test and ci-lint apply the same prefix
# for the same reason.
ci-build: ci-image
	$(CI_RUN) make STAMP=1 clean-bpf bpfman-build bpfman-shell-build

# Reproduce the workflow's check-vendor job locally. Verifies
# go.mod / go.sum / vendor are tidy. Runs on the host (no
# container) to match the GH job, which uses actions/setup-go
# directly on the runner rather than the bpfman-ci image. Like
# the upstream CI job this assumes a clean tree; commit or
# stash work-in-progress changes before invoking, otherwise
# `git diff --exit-code` will fail on them.
ci-check-vendor:
	go mod tidy
	go mod vendor
	git diff --exit-code

# Reproduce the workflow's check-fmt job locally. Same host /
# clean-tree contract as ci-check-vendor.
ci-check-fmt:
	$(MAKE) bpfman-fmt
	git diff --exit-code

# Reproduce the workflow's check-goimports job locally. Same host /
# clean-tree contract as ci-check-fmt.
ci-check-goimports:
	$(MAKE) bpfman-goimports
	git diff --exit-code

# Reproduce the workflow's lint job locally. Runs the full
# `make lint` umbrella (golangci-lint + hadolint + shellcheck +
# checkmake) inside the CI container.
ci-lint: ci-image
	$(CI_RUN) make clean-bpf lint

# Reproduce the workflow's check-vet job locally. Runs every vet
# pass (default + e2e/nsenter + cgo_sqlite) inside the CI container
# so the BPF embeds and CGO toolchain match what CI sees. Symmetric
# with ci-check-fmt and ci-check-vendor: a separate gate, not a
# side effect of bpfman-build.
ci-check-vet: ci-image
	$(CI_RUN) make clean-bpf bpfman-vet

# Reproduce the workflow's unit-test job locally. Source is
# mounted into the container so the test process sees the
# current working tree exactly as a host build would. Same Go
# cache volumes as ci-build for incremental-compile speed.
ci-test: ci-image
	$(CI_RUN) make clean-bpf test PARALLEL=1 STATIC=1 RACE=$(RACE)

# Reproduce the workflow's e2e job locally. The `e2e-export`
# stage produces a hermetic bundle at $(CI_E2E_BUNDLE); the
# self-contained e2e.test binary (BPF embedded, uprobe target
# merged) is then run on the host with sudo so it has the kernel
# privileges the e2e suite needs.
ci-test-e2e:
	$(RM) -r $(CI_E2E_BUNDLE)
	$(OCI_BIN) buildx build --target=e2e-export --output type=local,dest=$(CI_E2E_BUNDLE) -f $(CI_DOCKERFILE) --build-arg RACE=$(RACE) --build-arg EXTRA_TAGS=$(EXTRA_TAGS) $(CI_BUILDX_CACHE) .
	sudo $(if $(ISOLATED_RUNTIME),BPFMAN_E2E_ISOLATED_RUNTIME=$(ISOLATED_RUNTIME)) $(CI_E2E_BUNDLE)/bin/e2e.test -test.v -test.failfast -test.count=$(STRESS_COUNT) $(if $(PARALLEL),-test.parallel $(PARALLEL))

# Reproduce the workflow's e2e-scripts job locally. The REPL
# scripts under e2e/scripts/ and e2e/new/ are interpreted by the
# bpfman binary, so the bundle's bpfman + testdata are extracted
# directly into the source tree (the layout matches), and the
# scripts run via `make run-e2e-scripts` which assumes the
# artefacts are already in place. No outer sudo: the inner
# hack/test-e2e-scripts.sh shells out to `sudo bpfman-shell` per
# script invocation, which gets the kernel privileges it needs
# while leaving the rest of the make recipe unprivileged.
#
# Pre-clean the exact set of paths the bundle is about to write
# (bin/bpfman, bin/bpfman-shell, bin/e2e.test, and the BPF object
# tree). buildx --output overwrites individual files but does not
# prune anything stale, so leftover artefacts from a previous run
# could otherwise mask "didn't rebuild" bugs. golangci-lint under
# bin/ is preserved -- it has its own rule and re-fetching over
# the network is slow.
ci-test-e2e-scripts:
	$(RM) bin/bpfman bin/bpfman-shell bin/e2e.test
	$(MAKE) clean-bpf
	$(OCI_BIN) buildx build --target=e2e-export --output type=local,dest=. -f $(CI_DOCKERFILE) --build-arg RACE=$(RACE) --build-arg EXTRA_TAGS=$(EXTRA_TAGS) $(CI_BUILDX_CACHE) .
	$(MAKE) run-e2e-scripts

# Umbrella: run every CI pipeline locally. Cheap checks first
# (vendor/fmt) so failures surface fast; build before tests so
# the test job's container has a populated Go cache; e2e last.
#
# Run sequentially. CI gives each job its own runner, so the
# upstream workflow can fan out the e2e jobs in parallel; on a
# single dev box that fan-out collides on shared kernel state --
# bpffs mounts, dispatcher slot tables, the global program-id
# space, and the inode that uprobes attach to (for which both
# suites use the same e2e.test binary). Symptoms range from spurious
# attach failures to REPL counter assertions seeing the other
# suite's events. Don't `make -j ci-test-e2e ci-test-e2e-scripts`
# locally, and don't run them in two shells at once.
ci: ci-check-vendor ci-check-fmt ci-check-goimports ci-check-vet ci-build ci-lint ci-test ci-test-e2e ci-test-e2e-scripts

# ---------------------------------------------------------------------------
# gRPC integration test.
# ---------------------------------------------------------------------------
bpfman-test-grpc: build-image-dev
	BPFMAN_IMG=$(BPFMAN_IMG) OCI_BIN=$(OCI_BIN) scripts/test-grpc.sh


# ============================================================================
# PHONY declarations
# ============================================================================
# Grouped across several lines because checkmake does not parse
# .PHONY with backslash line continuations; each .PHONY line is a
# stand-alone declaration.
.PHONY: all build-all clean clean-mrproper help lint lint-dockerfile lint-go lint-hack lint-make
.PHONY: clean-bpf
.PHONY: bpfman-build clean-bpfman bpfman-compile bpfman-fmt bpfman-goimports bpfman-proto bpfman-test-grpc bpfman-vet
.PHONY: bpfman-shell-build bpfman-shell-compile clean-bpfman-shell
.PHONY: build-image build-image-amd64 build-image-arm64 build-image-csi-sanity build-image-dev build-image-nix build-image-openshift build-image-ppc64le build-image-s390x cosign-sign
.PHONY: ci ci-build ci-check-fmt ci-check-goimports ci-check-vendor ci-check-vet ci-image ci-lint ci-test ci-test-e2e ci-test-e2e-scripts
.PHONY: coverage clean-coverage coverage-func coverage-html coverage-open
.PHONY: doc doc-text
.PHONY: print-fedora-version print-go-version print-golangci-lint-version
.PHONY: build-e2e-scripts $(BIN_DIR)/e2e.test $(BIN_DIR)/e2e-grpc.test run-e2e-scripts test test-e2e test-e2e-grpc test-e2e-scripts test-examples
.PHONY: test-nsenter test-nsenter-amd64 test-nsenter-arm64 test-nsenter-cross test-nsenter-ppc64le test-nsenter-s390x
