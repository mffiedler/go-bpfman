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
# Image names and deployment knobs.
# ---------------------------------------------------------------------------
IMAGE_TAG ?= dev
BPFMAN_IMAGE ?= bpfman
STATS_READER_IMAGE ?= stats-reader
CSI_SANITY_IMAGE ?= csi-sanity

# ---------------------------------------------------------------------------
# CI build environment knobs. The `ci-*` make targets drive a
# Fedora-based docker image that mirrors what the GH workflows
# run, so a developer can reproduce CI locally with `make ci`.
# ---------------------------------------------------------------------------
CI_IMAGE       ?= bpfman-ci
CI_DOCKERFILE  ?= Dockerfile.ci
CI_E2E_OUTDIR  ?= ./out

# ---------------------------------------------------------------------------
# Test knobs.
# ---------------------------------------------------------------------------
PARALLEL ?=
# Optional regex passed to `-test.run` in test-e2e / test-e2e-scripts
# to narrow which tests execute. Empty by default = run all.
TEST ?=

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
# ---------------------------------------------------------------------------
GO_LDFLAGS := $(strip \
    $(if $(STATIC),-extldflags '-static') \
    -X $(VERSION_PKG).gitCommit=$(GIT_COMMIT) \
    -X $(VERSION_PKG).gitBranch=$(GIT_BRANCH) \
    -X $(VERSION_PKG).gitState=$(GIT_STATE) \
    -X $(VERSION_PKG).buildDate=$(BUILD_DATE) \
    -X $(VERSION_PKG).version=$(GIT_VERSION) \
    $(if $(IMAGE_REF),-X $(VERSION_PKG).imageRef=$(IMAGE_REF)) \
    $(if $(SIGNER_IDENTITY),-X $(VERSION_PKG).signerIdentity=$(SIGNER_IDENTITY)) \
    $(if $(OIDC_ISSUER),-X $(VERSION_PKG).oidcIssuer=$(OIDC_ISSUER)) \
    $(EXTRA_GO_LDFLAGS))

# ---------------------------------------------------------------------------
# Build tags.
# ---------------------------------------------------------------------------
STATIC_TAGS := osusergo,netgo
EXTRA_TAGS ?=
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
NSENTER_TEST_BIN ?= nsenter.test

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
BPF_SOURCES := $(wildcard dispatcher/bpf/*.bpf.c) $(wildcard e2e/testdata/bpf/*.bpf.c)
BPF_STAMP := .bpf-build-stamp

BPF_STAMP_DEPS := $(BPF_SOURCES) dispatcher/Makefile e2e/testdata/bpf/Makefile
BPF_STAMP_CMD  := $(MAKE) -C dispatcher && $(MAKE) -C e2e/testdata/bpf

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
# targets (build-image-dev, build-image-stats-reader, build-image-
# csi-sanity, build-image-openshift). Positioned just before the
# build context so caller flags override any preceding hard-coded
# flags that buildx/docker treats as last-wins.
EXTRA_DOCKER_BUILD_ARGS ?=
# Selects which Dockerfile the buildx targets use. Defaults to the
# in-tree Dockerfile.bpfman; override to test an alternative
# dockerfile without editing the recipe.
BPFMAN_DOCKERFILE ?= Dockerfile.bpfman

# True (1) when `docker` is the podman compat shim. Buildah/podman
# does not honor the per-Dockerfile <dockerfile>.dockerignore
# convention that buildkit reads automatically, so when running
# under podman the build-image-dev recipe passes --ignorefile
# explicitly to point at the per-file dockerignore. Buildkit-
# backed `docker` does not need this and ignores --ignorefile
# (it is a buildah/podman flag), so detection is required.
DOCKER_IS_PODMAN := $(shell docker --version 2>&1 | grep -ci podman)
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
	build-image-stats-reader build-image-csi-sanity build-image-openshift \
	ci-build ci-image ci-test ci-test-e2e ci-test-e2e-scripts \
	cosign-sign coverage clean

# Lint every Dockerfile / Containerfile with hadolint. The existing
# `# hadolint ignore=...` pragmas in the repo are already set up
# for this tool; adding the target wires it into CI.
LINT_DOCKERFILES := \
	Dockerfile.bpfman.dev \
	Dockerfile.bpfman \
	Dockerfile.ci \
	Dockerfile.csi-sanity \
	Containerfile.bpfman.openshift \
	examples/stats-reader/Dockerfile


# ============================================================================
# Targets
# ============================================================================

# ---------------------------------------------------------------------------
# Meta: default target, help, clean, version prints, bin directory.
# ---------------------------------------------------------------------------
all: bpfman-build

help:
	@echo "Build:"
	@echo "  build-all                   Build all binaries"
	@echo "  clean                       Remove all build artifacts"
	@echo "  clean-mrproper              Like 'clean', plus wipe Go's shared build/test/fuzz caches (~/.cache/go-build); affects all Go projects on this machine"
	@echo ""
	@echo "Testing:"
	@echo "  test                        Run all tests"
	@echo "  test-e2e                    Run e2e tests (requires root)"
	@echo "  test-e2e-scripts            Run REPL e2e scripts under e2e/scripts/ (requires root)"
	@echo "  test-examples               Run REPL scripts under examples/ (requires root)"
	@echo "  test-nsenter                Run nsenter tests (native amd64)"
	@echo "  test-nsenter-cross          Run nsenter tests on amd64/arm64/ppc64le/s390x"
	@echo "  test-nsenter-{arch}         Run nsenter tests for a single architecture"
	@echo "  lint                        Run golangci-lint"
	@echo "  coverage                    Generate coverage profile and show total"
	@echo "  coverage-func               Show coverage by function"
	@echo "  coverage-html               Generate HTML coverage report"
	@echo "  coverage-open               Generate and open HTML coverage report"
	@echo "  coverage-clean              Remove coverage artifacts"
	@echo ""
	@echo "Local CI reproducer (Dockerfile.ci):"
	@echo "  ci                          Run every ci-* target"
	@echo "  ci-build                    Compile bpfman binary inside the CI container"
	@echo "  ci-image                    Build the CI base image (loaded as bpfman-ci)"
	@echo "  ci-test                     Run unit tests inside the CI container"
	@echo "  ci-test-e2e                 Extract e2e test bundle and run it on the host (sudo)"
	@echo "  ci-test-e2e-scripts         Extract bundle to source tree and run REPL scripts (sudo)"
	@echo ""
	@echo "bpfman (with integrated CSI):"
	@echo "  bpfman-build                Build bpfman binary"
	@echo "  bpfman-compile              Compile bpfman (no fmt/vet/dispatchers)"
	@echo "  bpfman-clean                Remove generated files and binary"
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
	@echo "  build-image-stats-reader    Build stats-reader container image"
	@echo "  cosign-sign                 Sign a published image (requires BUILDX_METADATA_FILE)"
	@echo ""
	@echo "Documentation:"
	@echo "  doc                         Start pkgsite documentation server"
	@echo "  doc-text                    Print API documentation to stdout"
	@echo ""
	@echo "BPF:"
	@echo "  bpf-build                   Build all BPF programs"
	@echo "  bpf-clean                   Remove BPF build artifacts"
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

clean: bpfman-clean bpf-clean coverage-clean
	$(RM) -r $(BIN_DIR)

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

lint-go: bpf-build $(BIN_DIR)/golangci-lint
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
test: bpf-build
	$(strip go test -race $(EXTRA_GOFLAGS) $(if $(TEST_TAGS),-tags '$(TEST_TAGS)') $(if $(STATIC),-ldflags "$(GO_LDFLAGS)") -v $(if $(PARALLEL),-parallel $(PARALLEL)) ./...)

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
test-nsenter test-nsenter-amd64:
	@echo "=== nsenter: amd64 ==="
	$(strip CGO_ENABLED=1 go test -c $(EXTRA_GOFLAGS) $(if $(NSENTER_TAGS),-tags=$(NSENTER_TAGS)) -o $(NSENTER_TEST_BIN) ./ns/nsenter/)
	file $(NSENTER_TEST_BIN)
	sudo ./$(NSENTER_TEST_BIN) -test.v

test-nsenter-arm64 test-nsenter-ppc64le test-nsenter-s390x:
	NSENTER_TEST_BIN=$(NSENTER_TEST_BIN) NSENTER_TAGS=$(NSENTER_TAGS) \
		hack/test-nsenter-cross.sh $(@:test-nsenter-%=%)

test-nsenter-cross: $(addprefix test-nsenter-,$(NSENTER_ARCHES))

e2e/testdata/bin/call_malloc: e2e/testdata/bin/call_malloc.c
	$(CC) $(if $(STATIC),-static) -o $@ $<

test-e2e: bpf-build e2e/testdata/bin/call_malloc
	$(strip go test -c -race $(EXTRA_GOFLAGS) $(if $(E2E_TAGS),-tags=$(E2E_TAGS)) $(if $(STATIC),-ldflags "$(GO_LDFLAGS)") -o e2e.test ./e2e)
	cd e2e && sudo ../e2e.test -test.v -test.failfast $(if $(PARALLEL),-test.parallel $(PARALLEL)) $(if $(TEST),-test.run $(TEST))

# Run every REPL script under e2e/scripts/ against the built
# bpfman binary. Each script executes from e2e/ so testdata paths
# match the Go e2e tests. The target runs them sequentially,
# reports failures as it goes, and exits non-zero at the end if
# any script failed. Pass TEST=<name> to restrict to scripts whose
# filename contains <name>.
# Split into build + run so CI can extract pre-built artefacts
# from a hermetic container build (Dockerfile.ci's e2e-export
# stage) and invoke `run-e2e-scripts` directly on the runner
# without re-triggering the build deps. Local invocations of
# `test-e2e-scripts` still build first.
build-e2e-scripts: bpfman-compile bpf-build e2e/testdata/bin/call_malloc

run-e2e-scripts:
	@echo "Running REPL e2e scripts (requires root)..."
	BIN_DIR=$(BIN_DIR) hack/test-e2e-scripts.sh $(TEST)

test-e2e-scripts: build-e2e-scripts run-e2e-scripts

# Run every REPL script under examples/ against the built bpfman
# binary. The examples are load/attach/detach/unload
# walk-throughs; running them in CI catches drift between the
# shipped examples and the actual CLI surface. Pass TEST=<name> to
# restrict to scripts whose filename contains <name>.
test-examples: bpfman-compile bpf-build e2e/testdata/bin/call_malloc
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

coverage-clean:
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
bpfman-build: bpfman-fmt bpfman-vet bpf-build bpfman-compile

bpfman-fmt:
	go fmt ./...

bpfman-vet: bpf-build
	go vet ./...

# Compile bpfman without the dispatcher dependency. Used directly by
# container builds where dispatcher objects are already present.
bpfman-compile: | $(BIN_DIR)
	$(strip CGO_ENABLED=1 go build $(EXTRA_GOFLAGS) $(if $(BUILD_TAGS),-tags '$(BUILD_TAGS)') -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/bpfman ./cmd/bpfman)

bpfman-clean:
	$(RM) $(BIN_DIR)/bpfman e2e/testdata/bin/call_malloc

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
# BPF build targets.
# ---------------------------------------------------------------------------
bpf-build: $(BPF_STAMP)

$(BPF_STAMP): $(BPF_STAMP_DEPS)
	$(BPF_STAMP_CMD)
	touch $(BPF_STAMP)

bpf-clean:
	$(MAKE) -C dispatcher clean
	$(MAKE) -C e2e/testdata/bpf clean
	rm -f $(BPF_STAMP)

# ---------------------------------------------------------------------------
# Docker image builds.
# ---------------------------------------------------------------------------

# Build bpfman image from the host-built binary, using ubi9-minimal
# as the runtime base. Intended for local development and operator
# integration testing: the binary may be dynamically linked, and
# having a shell in the image aids `kubectl exec` debugging. The
# Dockerfile's default base is scratch; this target overrides it.
build-image-dev: bpfman-build
	docker build \
		$(if $(filter-out 0,$(DOCKER_IS_PODMAN)),--ignorefile=Dockerfile.bpfman.dev.dockerignore) \
		-t $(BPFMAN_IMAGE):$(IMAGE_TAG) \
		--build-arg BASE_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest \
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
#       BPFMAN_IMAGE=<registry/repo> \
#       IMAGE_TAG=$(GIT_COMMIT)
#       CI publish path: pushes a multi-arch manifest to the
#       registry, with SLSA build provenance (mode=max) and SBOM
#       attestations attached per platform.

# Pure-Nix OCI image, byte-reproducible and built without invoking
# a Docker daemon. Pulls the layered tarball that nix produces and
# `docker load`s it in one shot; --no-link keeps the workspace free
# of a stray result symlink that could collide with `nix build .`.
# See nix/image.nix for what is in the image and why.
build-image-nix:
	docker load < $$(nix build .#bpfman-image --print-out-paths --no-link)

build-image:
	docker buildx build \
		$(if $(PLATFORMS),--platform $(PLATFORMS)) \
		$(BUILDX_OUTPUT) \
		$(BUILDX_ATTEST) \
		$(if $(BUILDX_METADATA_FILE),--metadata-file=$(BUILDX_METADATA_FILE)) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GIT_BRANCH=$(GIT_BRANCH) \
		--build-arg GIT_VERSION=$(GIT_VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg EXTRA_GOFLAGS="$(EXTRA_GOFLAGS)" \
		--build-arg EXTRA_GO_LDFLAGS="$(EXTRA_GO_LDFLAGS)" \
		--build-arg IMAGE_REF="$(IMAGE_REF)" \
		--build-arg SIGNER_IDENTITY="$(SIGNER_IDENTITY)" \
		--build-arg OIDC_ISSUER="$(OIDC_ISSUER)" \
		-f $(BPFMAN_DOCKERFILE) \
		-t $(BPFMAN_IMAGE):$(IMAGE_TAG) \
		$(BUILDX_EXTRA_ARGS) .

# Per-arch presets pinning PLATFORMS to a single foreign arch.
# Each invocation builds one platform and --loads it into the local
# Docker store under the default $(BPFMAN_IMAGE):$(IMAGE_TAG) tag
# (e.g. bpfman:dev). The arch is implicit in the make target
# chosen, so the tag does not encode it; each invocation overwrites
# the previous one. To keep multiple arches loaded simultaneously,
# pass IMAGE_TAG explicitly.
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
#     BPFMAN_IMAGE=<registry/repo> \
#     IMAGE_TAG=latest \
#     BUILDX_METADATA_FILE=$${RUNNER_TEMP}/buildx-meta.json \
#     ...
#   make cosign-sign \
#     BPFMAN_IMAGE=<registry/repo> \
#     BUILDX_METADATA_FILE=$${RUNNER_TEMP}/buildx-meta.json
#
# Local usage (interactive OAuth signing identity):
#
#   nix shell nixpkgs#cosign      # cosign is not in the dev profile
#
#   make build-image \
#     PLATFORMS=linux/amd64 \
#     PUSH=1 \
#     BPFMAN_IMAGE=ttl.sh/frobware/go-bpfman-test \
#     BUILDX_METADATA_FILE=/tmp/buildx-meta.json
#
#   make cosign-sign \
#     BPFMAN_IMAGE=ttl.sh/frobware/go-bpfman-test \
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
	echo "Signing $(BPFMAN_IMAGE)@$$digest"; \
	cosign sign -y "$(BPFMAN_IMAGE)@$$digest"

# stats-reader example app
build-image-stats-reader:
	docker build -t $(STATS_READER_IMAGE):$(IMAGE_TAG) -f examples/stats-reader/Dockerfile $(EXTRA_DOCKER_BUILD_ARGS) .

# CSI conformance testing
build-image-csi-sanity:
	docker build -t $(CSI_SANITY_IMAGE):$(IMAGE_TAG) -f Dockerfile.csi-sanity $(EXTRA_DOCKER_BUILD_ARGS) .

build-image-openshift:
	docker build \
		-f $(OPENSHIFT_CONTAINERFILE) \
		$(if $(OPENSHIFT_BPF_BASE_IMAGE),--build-arg BPF_BASE_IMAGE=$(OPENSHIFT_BPF_BASE_IMAGE)) \
		$(if $(OPENSHIFT_BPF_INSTALL_CMD),--build-arg BPF_INSTALL_CMD="$(OPENSHIFT_BPF_INSTALL_CMD)") \
		--build-arg BUILD_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_BRANCH=$(GIT_BRANCH) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg BUILD_VERSION=$(GIT_VERSION) \
		-t $(BPFMAN_IMAGE):$(IMAGE_TAG) \
		$(EXTRA_DOCKER_BUILD_ARGS) .

# ---------------------------------------------------------------------------
# Local CI reproducer.
#
# `make ci` runs the same two pipelines the GH workflows run --
# build + unit-test inside a Fedora container, and an e2e job that
# extracts a static test bundle from the same dockerfile and runs
# it on the host with sudo. See Dockerfile.ci for the details of
# the build environment.
# ---------------------------------------------------------------------------

# Build the `base` stage of Dockerfile.ci as a tagged image, ready
# for `docker run` invocations against a mounted source tree. The
# `--load` is required for `docker run` to find the image in the
# local store.
ci-image:
	docker buildx build --target=base -t $(CI_IMAGE) -f $(CI_DOCKERFILE) --load .

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
ci-build: ci-image
	docker run --rm -v $(CURDIR):/src -w /src $(CI_IMAGE) \
		make bpfman-build

# Reproduce the workflow's unit-test job locally. Source is
# mounted into the container so the test process sees the
# current working tree exactly as a host build would.
ci-test: ci-image
	docker run --rm -v $(CURDIR):/src -w /src $(CI_IMAGE) \
		make test PARALLEL=1 STATIC=1

# Reproduce the workflow's e2e job locally. The `e2e-export`
# stage produces a hermetic bundle (binary + testdata) at
# $(CI_E2E_OUTDIR); the static binary is then run on the host
# with sudo so it has the kernel privileges the e2e suite needs.
ci-test-e2e:
	rm -rf $(CI_E2E_OUTDIR)
	docker buildx build --target=e2e-export --output type=local,dest=$(CI_E2E_OUTDIR) -f $(CI_DOCKERFILE) .
	cd $(CI_E2E_OUTDIR)/e2e && sudo ../e2e.test -test.v -test.failfast

# Reproduce the workflow's e2e-scripts job locally. The REPL
# scripts under e2e/scripts/ are interpreted by the bpfman
# binary, so the bundle's bpfman + testdata are extracted
# directly into the source tree (the layout matches), and the
# scripts run via `make run-e2e-scripts` which assumes the
# artefacts are already in place. No outer sudo: the inner
# hack/test-e2e-scripts.sh shells out to `sudo bpfman` per
# script invocation, which gets the kernel privileges it needs
# while leaving the rest of the make recipe unprivileged.
ci-test-e2e-scripts:
	docker buildx build --target=e2e-export --output type=local,dest=. -f $(CI_DOCKERFILE) .
	$(MAKE) run-e2e-scripts

# Umbrella: run every CI pipeline locally.
ci: ci-build ci-test ci-test-e2e ci-test-e2e-scripts

# ---------------------------------------------------------------------------
# gRPC integration test.
# ---------------------------------------------------------------------------
bpfman-test-grpc: build-image-dev
	BPFMAN_IMAGE=$(BPFMAN_IMAGE):$(IMAGE_TAG) scripts/test-grpc.sh


# ============================================================================
# PHONY declarations
# ============================================================================
# Grouped across several lines because checkmake does not parse
# .PHONY with backslash line continuations; each .PHONY line is a
# stand-alone declaration.
.PHONY: all build-all clean clean-mrproper help lint lint-dockerfile lint-go lint-hack lint-make
.PHONY: bpf-build bpf-clean
.PHONY: bpfman-build bpfman-clean bpfman-compile bpfman-fmt bpfman-proto bpfman-test-grpc bpfman-vet
.PHONY: build-image build-image-amd64 build-image-arm64 build-image-csi-sanity build-image-dev build-image-nix build-image-openshift build-image-ppc64le build-image-s390x build-image-stats-reader cosign-sign
.PHONY: ci ci-build ci-image ci-test ci-test-e2e ci-test-e2e-scripts
.PHONY: coverage coverage-clean coverage-func coverage-html coverage-open
.PHONY: doc doc-text
.PHONY: print-fedora-version print-go-version print-golangci-lint-version
.PHONY: build-e2e-scripts run-e2e-scripts test test-e2e test-e2e-scripts test-examples
.PHONY: test-nsenter test-nsenter-amd64 test-nsenter-arm64 test-nsenter-cross test-nsenter-ppc64le test-nsenter-s390x
