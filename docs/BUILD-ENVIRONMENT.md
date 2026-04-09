# Build environment and glibc version skew

## Problem

The bpfman binary is dynamically linked against glibc. The build
environment and the runtime container image use different distros
with different glibc versions. If the build-time glibc is newer than
the runtime glibc, the binary may reference symbol versions that do
not exist at runtime. This would cause the binary to fail to start
inside the container with an unresolved symbol error.

## Current state

Three distros are involved in the build pipeline:

| Stage                | Distro          | glibc version | Role                        |
|----------------------|-----------------|---------------|-----------------------------|
| BPF object build     | Fedora 43       | N/A           | clang, libbpf (bytecode)    |
| Go compilation / CI  | Ubuntu 24.04    | 2.39          | gcc, cross-compilers, tests |
| Container runtime    | UBI9 (RHEL 9)   | 2.34          | ships the binary            |

The BPF objects are architecture-independent bytecode; the Fedora
toolchain version is irrelevant at runtime. The concern is the Go
binary: it is built on Ubuntu 24.04 (glibc 2.39) and runs inside
UBI9 (glibc 2.34). The build-time glibc is newer than the runtime
glibc.

## Why it works today

Go's CGO usage is minimal. The only C code is the nsenter package's
constructor which calls setns(2) and a handful of standard libc
functions. These functions have stable symbol versions that predate
glibc 2.34. No symbol introduced after 2.34 is currently referenced.

## Why it is fragile

This guarantee is implicit. A future Go release, a new CGO
dependency, or a change to the nsenter C code could introduce a
reference to a newer glibc symbol. The failure mode is silent: CI
tests pass (running on Ubuntu with the correct glibc), but the
container image fails at startup with an unresolved symbol error.
There is no check in the pipeline that would catch this before the
image is pushed.

## Local development

Local development is not affected. The build and runtime environment
are the same machine, so glibc versions always match. This is true
on NixOS, Fedora, Ubuntu, and any other development host.

## Mitigation options

### Static linking (recommended)

Build the bpfman binary with `-extldflags '-static'`. This
eliminates the runtime glibc dependency entirely. The binary becomes
self-contained and runs in any Linux container regardless of distro,
including scratch and distroless images.

Tested locally: static linking works with CGO_ENABLED=1. The linker
emits warnings about glibc NSS functions (getaddrinfo, getpwnam_r,
getgrouplist) which use dlopen internally. These are harmless for
bpfman since it does not perform DNS resolution or user lookups via
libc.

The change required is adding `-extldflags '-static'` to the
GO_LDFLAGS in the Makefile.

### Use a build container matching the runtime

Build inside a UBI9-based container that provides the same glibc as
the runtime image. This ensures symbol version compatibility. The
cost is maintaining a UBI9-based build image with Go and cross-
compilers installed, which is more complex than using the Ubuntu
runners directly.

### Pin the CI runner to an older Ubuntu

Use an Ubuntu release whose glibc version is older than or equal to
UBI9's glibc 2.34. Ubuntu 22.04 ships glibc 2.35, which is still
newer than 2.34. There is no Ubuntu LTS release with glibc <= 2.34
that GitHub provides as a hosted runner. This option is not viable.

### Add a symbol version check to CI

After cross-compiling, run readelf or objdump on the binary to
extract the maximum GLIBC symbol version referenced. Fail the build
if it exceeds the runtime glibc version. This does not fix the
problem but would catch it before shipping a broken image.
