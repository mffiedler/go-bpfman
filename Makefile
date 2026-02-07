IMAGE_TAG ?= dev
BPFMAN_IMAGE ?= bpfman
KIND_CLUSTER ?= bpfman-deployment
NAMESPACE ?= bpfman
STATS_READER_IMAGE ?= stats-reader
BIN_DIR ?= bin

all: bpfman-fmt bpfman-vet bpfman-build

help:
	@echo "Build:"
	@echo "  build-all                   Build all binaries"
	@echo "  clean                       Remove all build artifacts"
	@echo "  docker-build-all            Build all container images"
	@echo ""
	@echo "Testing:"
	@echo "  test                        Run all tests"
	@echo "  test-e2e                    Run e2e tests (requires root)"
	@echo "  lint                        Run golangci-lint"
	@echo "  coverage                    Generate coverage profile and show total"
	@echo "  coverage-func               Show coverage by function"
	@echo "  coverage-html               Generate HTML coverage report"
	@echo "  coverage-open               Generate and open HTML coverage report"
	@echo "  coverage-clean              Remove coverage artifacts"
	@echo ""
	@echo "bpfman (with integrated CSI):"
	@echo "  bpfman-build                Build bpfman binary"
	@echo "  bpfman-build-portable       Build container-compatible binary (patchelf)"
	@echo "  bpfman-compile              Compile bpfman (no fmt/vet/dispatchers)"
	@echo "  bpfman-clean                Remove generated files and binary"
	@echo "  bpfman-delete               Remove bpfman from cluster"
	@echo "  bpfman-deploy               Deploy bpfman to KIND cluster"
	@echo "  bpfman-logs                 Follow bpfman logs"
	@echo "  bpfman-operator-deploy      Deploy Go bpfman to bpfman-operator cluster"
	@echo "  bpfman-proto                Generate protobuf/gRPC stubs"
	@echo "  bpfman-test-grpc            Run gRPC integration tests"
	@echo "  docker-build-bpfman         Build bpfman container image"
	@echo "  docker-build-bpfman-fast    Fast build using pre-built host binary"
	@echo "  docker-build-bpfman-upstream Build bpfman using upstream image as base"
	@echo "  docker-build-bpfman-upstream-fast Fast upstream build using host binary"
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
	@echo "Dispatchers:"
	@echo "  dispatchers-build           Build XDP/TC dispatcher BPF programs (host)"
	@echo "  dispatchers-docker          Build XDP/TC dispatcher BPF programs (Docker)"
	@echo "  dispatchers-clean           Remove dispatcher build artifacts"
	@echo "  dispatchers-docker-test     Build and test dispatcher files in container"
	@echo ""
	@echo "Combined:"
	@echo "  kind-undeploy-all           Remove all components from KIND cluster"
	@echo ""
	@echo "SQLite driver:"
	@echo "  The default SQLite driver is modernc.org/sqlite (pure Go)."
	@echo "  To use mattn/go-sqlite3 (CGO) instead, pass -tags cgo_sqlite:"
	@echo "    go build -tags cgo_sqlite ./..."
	@echo "    go test -tags cgo_sqlite ./..."

docker-build-all: docker-build-bpfman docker-build-bpfman-upstream docker-build-stats-reader docker-build-csi-sanity

clean: bpfman-clean dispatchers-clean coverage-clean
	$(RM) -r $(BIN_DIR)

test:
	go test -race -v ./...

lint:
	golangci-lint run

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

test-e2e:
	@echo "Running e2e tests (requires root)..."
	go test -race -count=1 -tags=e2e -v ./e2e/...

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

# bpfman targets
# Note: bpfman-proto is not a dependency here since pb files are committed.
# Run 'make bpfman-proto' explicitly after modifying proto/bpfman.proto.
# CGO is required for the nsenter package which uses a C constructor to call
# setns() before Go runtime starts (needed for uprobe container attachment).
# For daily development use bpfman-all which also runs fmt and vet.
bpfman-build: ensure-dispatchers bpfman-compile

bpfman-fmt:
	go fmt ./...

bpfman-vet: ensure-dispatchers
	go vet ./...

# Compile bpfman without the dispatcher dependency. Used directly by
# container builds where dispatcher objects are already present.
bpfman-compile: | $(BIN_DIR)
	CGO_ENABLED=1 go build -mod=vendor -o $(BIN_DIR)/bpfman ./cmd/bpfman

# Ensure bin directory exists
$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

# Build binary patched for use in containers (fixes nix interpreter/rpath).
# Requires patchelf to be installed.
bpfman-build-portable: bpfman-build
	@if ! command -v patchelf >/dev/null 2>&1; then \
		echo "Error: patchelf is required but not installed"; \
		exit 1; \
	fi
	cp $(BIN_DIR)/bpfman $(BIN_DIR)/bpfman-portable
	patchelf --set-interpreter /lib64/ld-linux-x86-64.so.2 \
		--set-rpath /lib64:/lib/x86_64-linux-gnu \
		$(BIN_DIR)/bpfman-portable
	@echo "Built $(BIN_DIR)/bpfman-portable (container-compatible)"

bpfman-clean:
	$(RM) $(BIN_DIR)/bpfman $(BIN_DIR)/bpfman-portable

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

docker-build-bpfman: testdata/stats.o
	docker build -t $(BPFMAN_IMAGE):$(IMAGE_TAG) -f Dockerfile.bpfman .

# Fast build: copy pre-built binary from host (skips in-container compilation)
# Requires: make bpfman-build-portable first
docker-build-bpfman-fast: bpfman-build-portable testdata/stats.o
	docker build -t $(BPFMAN_IMAGE):$(IMAGE_TAG) -f Dockerfile.bpfman-fast .

# Build bpfman using upstream image as base (for operator integration testing)
docker-build-bpfman-upstream: bpfman-build
	docker build -t $(BPFMAN_IMAGE):$(IMAGE_TAG) -f Dockerfile.bpfman-upstream .

# Fast build using upstream image as base
docker-build-bpfman-upstream-fast: bpfman-build-portable
	docker build -t $(BPFMAN_IMAGE):$(IMAGE_TAG) -f Dockerfile.bpfman-upstream-fast .

bpfman-kind-load: docker-build-bpfman
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
bpfman-operator-deploy: docker-build-bpfman-upstream-fast
	docker tag $(BPFMAN_IMAGE):$(IMAGE_TAG) $(BPFMAN_IMAGE):latest
	kind load docker-image $(BPFMAN_IMAGE):latest --name $(KIND_CLUSTER)
	kubectl rollout restart daemonset/bpfman-daemon -n $(NAMESPACE)
	kubectl rollout status daemonset/bpfman-daemon -n $(NAMESPACE) --timeout=60s

bpfman-test-grpc: docker-build-bpfman
	BPFMAN_IMAGE=$(BPFMAN_IMAGE):$(IMAGE_TAG) scripts/test-grpc.sh

# bpfman testdata
BPFMAN_HACKS_DIR ?= $(HOME)/src/github.com/frobware/bpfman-hacks

testdata/stats.o: $(BPFMAN_HACKS_DIR)/stats/bpf/stats.o
	mkdir -p testdata
	cp $< $@

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

# Dispatcher targets
dispatchers-build:
	$(MAKE) -C dispatcher

dispatchers-clean:
	$(MAKE) -C dispatcher clean
	rm -f $(DISPATCHER_OBJECTS) $(DISPATCHER_STAMP)

# Smart Docker-based dispatcher build - only rebuilds when sources change
DISPATCHER_OBJECTS := dispatcher/tc_dispatcher.bpf.o dispatcher/xdp_dispatcher_v1.bpf.o dispatcher/xdp_dispatcher_v2.bpf.o
DISPATCHER_SOURCES := $(patsubst dispatcher/%.bpf.o,dispatcher/bpf/%.bpf.c,$(DISPATCHER_OBJECTS))
DISPATCHER_STAMP := .dispatcher-docker-stamp

# Phony target to ensure dispatcher objects are built
.PHONY: ensure-dispatchers
ensure-dispatchers: $(DISPATCHER_STAMP)
	@# Verify all dispatcher objects exist
	@for obj in $(DISPATCHER_OBJECTS); do \
		if [ ! -f "$$obj" ]; then \
			echo "Error: $$obj not found after Docker build"; \
			exit 1; \
		fi; \
	done

# Convenience target for building dispatchers with Docker
dispatchers-docker: $(DISPATCHER_STAMP)

# Use a stamp file to track when dispatcher objects are built
$(DISPATCHER_STAMP): $(DISPATCHER_SOURCES) dispatcher/Makefile Dockerfile.dispatchers Makefile
	docker build -f Dockerfile.dispatchers --target testable -t bpfman-dispatchers-test:latest .
	docker rm dispatcher-temp 2>/dev/null || true
	docker create --name dispatcher-temp bpfman-dispatchers-test:latest
	docker cp dispatcher-temp:/dispatcher/ ./
	docker rm dispatcher-temp
	@echo "Extracted updated BPF objects to ./dispatcher/"
	touch $(DISPATCHER_STAMP)

dispatchers-docker-test:
	docker build -f Dockerfile.dispatchers --target testable -t bpfman-dispatchers-test:latest .
	docker run --rm bpfman-dispatchers-test:latest

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
	dispatchers-build \
	dispatchers-clean \
	dispatchers-docker-build \
	dispatchers-docker-extract \
	dispatchers-docker-test \
	doc \
	doc-text \
	docker-build-all \
	docker-build-bpfman \
	docker-build-bpfman-upstream \
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
	test
