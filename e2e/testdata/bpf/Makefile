# SPDX-License-Identifier: Apache-2.0
# Copyright Authors of bpfman

# Makefile for test BPF programs.

LIBBPF_CFLAGS := $(shell pkg-config --cflags libbpf)
BPF_CFLAGS ?=

TARGET_ARCH ?= $(shell uname -m)

ifeq ($(TARGET_ARCH),x86_64)
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

SOURCES := $(wildcard *.bpf.c)
OBJECTS := $(SOURCES:.bpf.c=.bpf.o)

all: $(OBJECTS)

%.bpf.o: %.bpf.c Makefile
	clang $(LIBBPF_CFLAGS) $(BPF_CFLAGS) -g -O2 -target bpfel -c $(TARGET_ARCH_DEFINE) $< -o $@

clean:
	rm -f $(OBJECTS)

.PHONY: all clean
