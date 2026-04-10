# SPDX-License-Identifier: Apache-2.0
# Copyright Authors of bpfman

# Makefile for eBPF dispatcher compilation.

LIBBPF_CFLAGS := $(shell pkg-config --cflags libbpf)
BPF_CFLAGS ?=

# Dynamic architecture detection using uname
TARGET_ARCH ?= $(shell uname -m)

ifeq ($(TARGET_ARCH),x86_64)
  TARGET_ARCH_DEFINE := -D__TARGET_ARCH_x86
else ifeq ($(TARGET_ARCH),i686)
  TARGET_ARCH_DEFINE := -D__TARGET_ARCH_x86
else ifeq ($(TARGET_ARCH),aarch64)
  TARGET_ARCH_DEFINE := -D__TARGET_ARCH_arm64
else ifeq ($(TARGET_ARCH),powerpc64le)
  TARGET_ARCH_DEFINE := -D__TARGET_ARCH_powerpc
else ifeq ($(TARGET_ARCH),s390x)
  TARGET_ARCH_DEFINE := -D__TARGET_ARCH_s390
else
  $(error Unsupported architecture: $(TARGET_ARCH))
endif

# xdp_dispatcher_v1.bpf.c is kept in-tree as historical reference
# only.  It is not embedded by any Go code and is not installed into
# the package directory, so excluding it from SOURCES skips the
# clang invocation entirely.
SOURCES := $(filter-out bpf/xdp_dispatcher_v1.bpf.c,$(wildcard bpf/*.bpf.c))
# Use OUT_DIR if provided, otherwise use the directory containing this Makefile.
OUT_DIR ?= $(CURDIR)
OBJECTS := $(addprefix $(OUT_DIR)/,$(SOURCES:.bpf.c=.bpf.o))
DEP_FILES := $(addprefix $(OUT_DIR)/,$(SOURCES:.bpf.c=.bpf.d))

# Go package directory for embedding
GO_PKG_DIR := .

all: $(OBJECTS) install

install: $(OBJECTS)
	cp $(OUT_DIR)/bpf/xdp_dispatcher_v2.bpf.o $(GO_PKG_DIR)/
	cp $(OUT_DIR)/bpf/tc_dispatcher.bpf.o $(GO_PKG_DIR)/

# Explicit static pattern rule handles Docker builds without disabling builtin rules

$(OUT_DIR)/bpf/%.bpf.o: bpf/%.bpf.c Makefile | $(OUT_DIR)
	clang $(LIBBPF_CFLAGS) $(BPF_CFLAGS) -g -O2 -target bpfel -c $(TARGET_ARCH_DEFINE) -MD -MP -MF$(OUT_DIR)/bpf/$*.bpf.d $< -o $@

# Explicit rules for Docker builds (matches pattern rule)
tc_dispatcher.bpf.o xdp_dispatcher_v2.bpf.o: %.bpf.o: bpf/%.bpf.c Makefile
	clang $(LIBBPF_CFLAGS) $(BPF_CFLAGS) -g -O2 -target bpfel -c $(TARGET_ARCH_DEFINE) -MD -MP -MF$*.bpf.d $< -o $@

$(OUT_DIR):
	mkdir -p $(OUT_DIR)

.PHONY: clean
clean:
	rm -f $(OBJECTS) $(DEP_FILES)
	rm -f $(GO_PKG_DIR)/*.bpf.o

.PHONY: test-pkgconfig
test-pkgconfig:
	pkg-config --exists libbpf

.PHONY: debug
debug:
	@echo "TARGET_ARCH: $(TARGET_ARCH)"
	@echo "TARGET_ARCH_DEFINE: $(TARGET_ARCH_DEFINE)"
	@echo "LIBBPF_CFLAGS: $(LIBBPF_CFLAGS)"
	@which clang
	@ls -la *.bpf.c

-include $(DEP_FILES)
