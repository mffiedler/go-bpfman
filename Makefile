# Make helper: literal comma for use inside $(if) expansions.
comma := ,

# Tool versions — single source of truth for CI and Docker builds.
FEDORA_VERSION ?= 43
GO_VERSION ?= 1.25
GOLANGCI_LINT_VERSION ?= v2.11.2

IMAGE_TAG ?= dev
BPFMAN_IMAGE ?= bpfman
KIND_CLUSTER ?= bpfman-deployment
NAMESPACE ?= bpfman
STATS_READER_IMAGE ?= stats-reader
BIN_DIR ?= bin

all: bpfman-build

print-go-version:
	@echo $(GO_VERSION)

print-fedora-version:
	@echo $(FEDORA_VERSION)

print-golangci-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

$(BIN_DIR)/golangci-lint: | $(BIN_DIR)
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(abspath $(BIN_DIR)) $(GOLANGCI_LINT_VERSION)

help:
	@echo "Build:"
	@echo "  build-all                   Build all binaries"
	@echo "  clean                       Remove all build artifacts"
	@echo "  docker-build-all            Build all container images"
	@echo ""
	@echo "Testing:"
	@echo "  test                        Run all tests"
	@echo "  test-e2e                    Run e2e tests (requires root)"
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
	@echo "bpfman (with integrated CSI):"
	@echo "  bpfman-build                Build bpfman binary"
	@echo "  bpfman-compile              Compile bpfman (no fmt/vet/dispatchers)"
	@echo "  bpfman-clean                Remove generated files and binary"
	@echo "  bpfman-delete               Remove bpfman from cluster"
	@echo "  bpfman-deploy               Deploy bpfman to KIND cluster"
	@echo "  bpfman-logs                 Follow bpfman logs"
	@echo "  bpfman-operator-deploy      Deploy Go bpfman to bpfman-operator cluster"
	@echo "  bpfman-proto                Generate protobuf/gRPC stubs"
	@echo "  bpfman-test-grpc            Run gRPC integration tests"
	@echo "  build-image                 Build a local bpfman image (alias for docker-build-bpfman-multiarch)"
	@echo "  docker-build-bpfman-local   Build bpfman image from host-built binary"
	@echo "  docker-build-bpfman-multiarch  Buildx multi-arch build (PLATFORMS=, PUSH=)"
	@echo ""
	@echo "Example stats-reader app:"
	@echo "  docker-build-stats-reader   Build stats-reader container image"
	@echo "  stats-reader-delete         Remove stats-reader pod"
	@echo "  stats-reader-deploy         Deploy stats-reader pod"
	@echo "  stats-reader-logs           Follow stats-reader logs"
	@echo ""
	@echo "CSI conformance testing:"
	@echo "  docker-build-csi-sanity     Build csi-sanity container image"
	@echo ""
	@echo "KIND cluster:"
	@echo "  kind-create                 Create KIND cluster with bpffs mounted"
	@echo "  kind-delete                 Delete KIND cluster"
	@echo ""
	@echo "Documentation:"
	@echo "  doc                         Start pkgsite documentation server"
	@echo "  doc-text                    Print API documentation to stdout"
	@echo ""
	@echo "BPF:"
	@echo "  bpf-build                   Build all BPF programs (Docker by default)"
	@echo "  bpf-clean                   Remove BPF build artifacts"
	@echo "  Set BPF_USE_HOST=1 to use host toolchain instead of Docker."
	@echo ""
	@echo "Combined:"
	@echo "  kind-undeploy-all           Remove all components from KIND cluster"
	@echo ""
	@echo "SQLite driver:"
	@echo "  The default SQLite driver is modernc.org/sqlite (pure Go)."
	@echo "  To use mattn/go-sqlite3 (CGO) instead, pass -tags cgo_sqlite:"
	@echo "    go build -tags cgo_sqlite ./..."
	@echo "    go test -tags cgo_sqlite ./..."

docker-build-all: docker-build-bpfman-local docker-build-stats-reader docker-build-csi-sanity

clean: bpfman-clean bpf-clean coverage-clean
	$(RM) -r $(BIN_DIR)

PARALLEL ?=

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
# assignments (make STATIC=0) otherwise win over file-level ones.
override STATIC := $(filter 1,$(STATIC))

test: bpf-build
	go test -race $(if $(STATIC),-tags '$(STATIC_TAGS)' -ldflags "$(GO_LDFLAGS)") -v $(if $(PARALLEL),-parallel $(PARALLEL)) ./...

lint: bpf-build $(BIN_DIR)/golangci-lint
	$(BIN_DIR)/golangci-lint run

# Coverage targets
COVERAGE_DIR ?= .coverage
COVERAGE_PROFILE ?= $(COVERAGE_DIR)/coverage.out
COVERAGE_HTML ?= $(COVERAGE_DIR)/coverage.html

coverage:
	@mkdir -p $(COVERAGE_DIR)
	@go test -coverprofile=$(COVERAGE_PROFILE) ./... 2>&1 | grep -v "no test files" | grep -v "no such tool" | grep -v "^#"
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

NSENTER_ARCHES ?= amd64 arm64 ppc64le s390x

NSENTER_TEST_BIN ?= nsenter.test

test-nsenter test-nsenter-amd64:
	@echo "=== nsenter: amd64 ==="
	CGO_ENABLED=1 go test -c -tags=nsenter -o $(NSENTER_TEST_BIN) ./ns/nsenter/
	file $(NSENTER_TEST_BIN)
	sudo ./$(NSENTER_TEST_BIN) -test.v

test-nsenter-arm64 test-nsenter-ppc64le test-nsenter-s390x:
	@goarch=$(@:test-nsenter-%=%); \
	case $$goarch in \
		arm64)   prefix=aarch64;     qemu_arch=aarch64 ;; \
		ppc64le) prefix=powerpc64le; qemu_arch=ppc64le ;; \
		s390x)   prefix=s390x;       qemu_arch=s390x ;; \
	esac; \
	cc=$$(command -v $${prefix}-unknown-linux-gnu-gcc 2>/dev/null || \
	      command -v $${prefix}-linux-gnu-gcc 2>/dev/null || true); \
	if [ -z "$$cc" ]; then \
		echo "error: no cross-compiler for $$goarch" >&2; \
		echo "  tried: $${prefix}-unknown-linux-gnu-gcc (nix)" >&2; \
		echo "  tried: $${prefix}-linux-gnu-gcc (distro)" >&2; \
		exit 1; \
	fi; \
	qemu="qemu-$$qemu_arch"; \
	sysroot=""; \
	if [ -d "/usr/$${prefix}-linux-gnu" ]; then \
		sysroot="/usr/$${prefix}-linux-gnu"; \
		qemu="$$qemu -L $$sysroot"; \
	fi; \
	echo "=== nsenter: $$goarch (CC=$$cc, exec=$$qemu) ==="; \
	CGO_ENABLED=1 GOOS=linux GOARCH=$$goarch CC="$$cc" \
		go test -c -tags=nsenter -o $(NSENTER_TEST_BIN) ./ns/nsenter/; \
	file $(NSENTER_TEST_BIN); \
	sudo QEMU_LD_PREFIX="$$sysroot" \
		$$qemu ./$(NSENTER_TEST_BIN) -test.v

test-nsenter-cross: $(addprefix test-nsenter-,$(NSENTER_ARCHES))

e2e/testdata/bin/call_malloc: e2e/testdata/bin/call_malloc.c
	$(CC) -O0 $(if $(STATIC),-static) -o $@ $<

test-e2e: bpf-build e2e/testdata/bin/call_malloc
	@echo "Compiling e2e test binary..."
	go test -c -race -tags=e2e$(if $(STATIC),$(comma)$(STATIC_TAGS)) $(if $(STATIC),-ldflags "$(GO_LDFLAGS)") -o e2e.test ./e2e
	@echo "Running e2e tests (requires root)..."
	cd e2e && sudo ../e2e.test -test.failfast $(if $(PARALLEL),-test.parallel $(PARALLEL)) $(if $(TEST),-test.run $(TEST))

# Documentation
DOC_PORT ?= 6060

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

# Version information injected at build time.
VERSION_PKG := github.com/frobware/go-bpfman/version
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
GIT_BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
GIT_STATE ?= $(shell if git diff --quiet 2>/dev/null; then echo clean; else echo dirty; fi)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)

GO_LDFLAGS := $(if $(STATIC),-extldflags '-static') \
              -X $(VERSION_PKG).gitCommit=$(GIT_COMMIT) \
              -X $(VERSION_PKG).gitBranch=$(GIT_BRANCH) \
              -X $(VERSION_PKG).gitState=$(GIT_STATE) \
              -X $(VERSION_PKG).buildDate=$(BUILD_DATE) \
              -X $(VERSION_PKG).version=$(GIT_VERSION)

# bpfman targets
# Note: bpfman-proto is not a dependency here since pb files are committed.
# Run 'make bpfman-proto' explicitly after modifying proto/bpfman.proto.
# CGO is required for the ns/nsenter package which uses a C constructor to call
# setns() before Go runtime starts (needed for uprobe container attachment).
bpfman-build: bpfman-fmt bpfman-vet bpf-build bpfman-compile

bpfman-fmt:
	go fmt ./...

bpfman-vet: bpf-build
	go vet ./...

# Compile bpfman without the dispatcher dependency. Used directly by
# container builds where dispatcher objects are already present.
bpfman-compile: | $(BIN_DIR)
STATIC_TAGS := osusergo,netgo

bpfman-compile: | $(BIN_DIR)
	CGO_ENABLED=1 go build $(if $(STATIC),-tags '$(STATIC_TAGS)') -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/bpfman ./cmd/bpfman

# Ensure bin directory exists
$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

bpfman-clean:
	$(RM) $(BIN_DIR)/bpfman e2e/testdata/bin/call_malloc

# Proto generation for bpfman gRPC API
BPFMAN_PROTO_DIR := proto
BPFMAN_PB_DIR := server/pb

bpfman-proto: $(BPFMAN_PB_DIR)/bpfman.pb.go $(BPFMAN_PB_DIR)/bpfman_grpc.pb.go

$(BPFMAN_PB_DIR)/bpfman.pb.go $(BPFMAN_PB_DIR)/bpfman_grpc.pb.go: $(BPFMAN_PROTO_DIR)/bpfman.proto
	mkdir -p $(BPFMAN_PB_DIR)
	protoc --go_out=$(BPFMAN_PB_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(BPFMAN_PB_DIR) --go-grpc_opt=paths=source_relative \
		--proto_path=$(BPFMAN_PROTO_DIR) \
		$<

# Build bpfman image from the host-built binary, using ubi9-minimal
# as the runtime base. Intended for local development and operator
# integration testing: the binary may be dynamically linked, and
# having a shell in the image aids `kubectl exec` debugging. The
# Dockerfile's default base is scratch; this target overrides it.
docker-build-bpfman-local: bpfman-build
	docker build -t $(BPFMAN_IMAGE):$(IMAGE_TAG) \
		--build-arg BASE_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest \
		-f Dockerfile.bpfman.host .

# Multi-architecture buildx-native image build.
#
# Modes (selected automatically by the variable knobs below):
#
#   make docker-build-bpfman-multiarch
#       Default: native arch only, loaded into the local Docker
#       store. Suitable for daily KIND work that does not require a
#       shell-in-pod (use docker-build-bpfman-local for that).
#
#   make docker-build-bpfman-multiarch PLATFORMS=linux/arm64
#       Single foreign arch, loaded into the local Docker store
#       (requires host binfmt support to actually run the binary).
#
#   make docker-build-bpfman-multiarch \
#       PLATFORMS=linux/amd64,linux/arm64,linux/ppc64le,linux/s390x
#       Multi-arch, cache-only build (no output). Useful as a "does
#       it all compile?" sanity check; the manifest stays in the
#       BuildKit cache because the local Docker store cannot hold a
#       multi-arch manifest.
#
#   make docker-build-bpfman-multiarch \
#       PLATFORMS=linux/amd64,linux/arm64,linux/ppc64le,linux/s390x \
#       PUSH=1 \
#       BPFMAN_IMAGE=ttl.sh/frobware/go-bpfman \
#       IMAGE_TAG=$(GIT_COMMIT)
#       CI publish: pushes a multi-arch manifest to the registry,
#       with SLSA build provenance (mode=max) and SBOM attestations
#       attached per platform.
#
# STATIC linkage is intentionally NOT a knob here. The Dockerfile
# hardcodes `make bpfman-compile STATIC=1` because the final stage is
# `scratch` and the two are coupled: a dynamically linked binary
# would crash immediately on a libc-less base. If you need a
# non-static binary (FIPS Go toolchains, dynamic-glibc bases), build
# on the host and package via Dockerfile.bpfman.host instead.
#
# Multi-platform builds require a docker-container or remote buildx
# builder. CI workflows use docker/setup-buildx-action which
# provisions one automatically; locally, run `docker buildx create
# --driver docker-container --use` once.
PLATFORMS         ?=
PUSH              ?=
BUILDX_EXTRA_ARGS ?=

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

# Short alias: `make build-image` is the obvious thing to type when
# you just want a local bpfman image. It is identical to running
# docker-build-bpfman-multiarch with no overrides.
build-image: docker-build-bpfman-multiarch

docker-build-bpfman-multiarch:
	docker buildx build \
		$(if $(PLATFORMS),--platform $(PLATFORMS)) \
		$(BUILDX_OUTPUT) \
		$(BUILDX_ATTEST) \
		$(BUILDX_EXTRA_ARGS) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GIT_BRANCH=$(GIT_BRANCH) \
		--build-arg GIT_VERSION=$(GIT_VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-f Dockerfile.bpfman.multiarch \
		-t $(BPFMAN_IMAGE):$(IMAGE_TAG) .

bpfman-kind-load: docker-build-bpfman-local
	kind load docker-image $(BPFMAN_IMAGE):$(IMAGE_TAG) --name $(KIND_CLUSTER)

bpfman-deploy: bpfman-kind-load
	kubectl apply -f manifests/csidriver.yaml -f manifests/bpfman.yaml
	kubectl -n $(NAMESPACE) wait --for=condition=Ready pod -l app=bpfman-daemon-go --timeout=60s

bpfman-delete:
	kubectl delete -f manifests/bpfman.yaml -f manifests/csidriver.yaml --ignore-not-found

bpfman-logs:
	kubectl -n $(NAMESPACE) logs -l app=bpfman-daemon-go -c bpfman -f

bpfman-deploy-test: bpfman-kind-load
	kubectl apply -f manifests/bpfman-test-pod.yaml
	kubectl wait --for=condition=Ready pod/bpfman-test --timeout=30s

bpfman-delete-test:
	kubectl delete -f manifests/bpfman-test-pod.yaml --ignore-not-found

# Deploy Go bpfman to an existing bpfman-operator deployment (replaces Rust bpfman)
bpfman-operator-deploy: docker-build-bpfman-local
	docker tag $(BPFMAN_IMAGE):$(IMAGE_TAG) $(BPFMAN_IMAGE):latest
	kind load docker-image $(BPFMAN_IMAGE):latest --name $(KIND_CLUSTER)
	kubectl rollout restart daemonset/bpfman-daemon -n $(NAMESPACE)
	kubectl rollout status daemonset/bpfman-daemon -n $(NAMESPACE) --timeout=60s

bpfman-test-grpc: docker-build-bpfman-local
	BPFMAN_IMAGE=$(BPFMAN_IMAGE):$(IMAGE_TAG) scripts/test-grpc.sh

# stats-reader example app
docker-build-stats-reader:
	docker build -t $(STATS_READER_IMAGE):$(IMAGE_TAG) -f examples/stats-reader/Dockerfile .

stats-reader-kind-load: docker-build-stats-reader
	kind load docker-image $(STATS_READER_IMAGE):$(IMAGE_TAG) --name $(KIND_CLUSTER)

stats-reader-deploy: stats-reader-kind-load
	kubectl apply -f manifests/stats-reader.yaml
	kubectl wait --for=condition=Ready pod/stats-reader --timeout=30s

stats-reader-delete:
	kubectl delete -f manifests/stats-reader.yaml --ignore-not-found

stats-reader-logs:
	kubectl logs -f stats-reader

# CSI conformance testing
CSI_SANITY_IMAGE ?= csi-sanity

docker-build-csi-sanity:
	docker build -t $(CSI_SANITY_IMAGE):$(IMAGE_TAG) -f Dockerfile.csi-sanity .

# KIND cluster management
kind-create:
	kind create cluster --name $(KIND_CLUSTER) --config kind-config.yaml
	@echo "Mounting bpffs on KIND nodes..."
	@for node in $$(kind get nodes --name $(KIND_CLUSTER)); do \
		docker exec $$node mount -t bpf bpf /sys/fs/bpf 2>/dev/null || true; \
	done
	@echo "KIND cluster $(KIND_CLUSTER) created with bpffs mounted"

kind-delete:
	kind delete cluster --name $(KIND_CLUSTER)

# BPF build targets
#
# Default: build all BPF programs (dispatchers + e2e testdata) via Docker.
# Set BPF_USE_HOST=1 to use the host toolchain instead.
# Set BPF_DOCKERFILE to select a different BPF builder Dockerfile;
# downstream Konflux builds set BPF_DOCKERFILE=Dockerfile.bpf.openshift
# to substitute a UBI-based builder. The Dockerfile contract is the
# output layout under /output/, not the build environment.
# Set DOCKER_BUILD_ARGS for additional docker build flags (e.g., cache options).

DOCKER_BUILD_ARGS ?=
BPF_DOCKERFILE ?= Dockerfile.bpf

BPF_SOURCES := $(wildcard dispatcher/bpf/*.bpf.c) $(wildcard e2e/testdata/bpf/*.bpf.c)
BPF_STAMP := .bpf-build-stamp

.PHONY: bpf-build bpf-clean

bpf-build: $(BPF_STAMP)

ifdef BPF_USE_HOST
$(BPF_STAMP): $(BPF_SOURCES) dispatcher/Makefile e2e/testdata/bpf/Makefile
	$(MAKE) -C dispatcher
	$(MAKE) -C e2e/testdata/bpf
	touch $(BPF_STAMP)
else
$(BPF_STAMP): $(BPF_SOURCES) dispatcher/Makefile e2e/testdata/bpf/Makefile $(BPF_DOCKERFILE)
	docker build -f $(BPF_DOCKERFILE) --target artifacts --output type=local,dest=. $(DOCKER_BUILD_ARGS) .
	touch $(BPF_STAMP)
endif

bpf-clean:
	$(MAKE) -C dispatcher clean
	$(MAKE) -C e2e/testdata/bpf clean
	rm -f $(BPF_STAMP)

# Combined targets
kind-undeploy-all: stats-reader-delete bpfman-delete

.PHONY: \
	bpfman-build \
	bpfman-clean \
	bpfman-delete \
	bpfman-delete-test \
	bpfman-deploy \
	bpfman-deploy-test \
	bpfman-kind-load \
	bpfman-logs \
	bpfman-proto \
	bpfman-test-grpc \
	build-all \
	clean \
	coverage \
	coverage-clean \
	coverage-func \
	coverage-html \
	coverage-open \
	bpf-build \
	bpf-clean \
	doc \
	doc-text \
	docker-build-all \
	build-image \
	docker-build-bpfman-local \
	docker-build-bpfman-multiarch \
	docker-build-csi-sanity \
	docker-build-stats-reader \
	help \
	kind-create \
	kind-delete \
	kind-undeploy-all \
	lint \
	stats-reader-delete \
	stats-reader-deploy \
	stats-reader-logs \
	test-e2e \
	test \
	test-nsenter \
	test-nsenter-amd64 \
	test-nsenter-arm64 \
	test-nsenter-cross \
	test-nsenter-ppc64le \
	test-nsenter-s390x
