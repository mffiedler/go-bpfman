{
  description = "go-bpfman development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

    # Separate pin used only for building the e2e kernel module
    # (bpfman-e2e-targets-kmod). The kernel headers a kmod is
    # built against must match the running kernel's vermagic
    # exactly (including patch level), otherwise the kernel
    # rejects insmod. The branch URL "nixos-25.11" advances
    # over time, so a matching consumer would have to update
    # both their NixOS pin and this one in lockstep. We pin to
    # an exact revision instead so the kmod is reproducibly
    # tied to one kernel version. Hosts running a different
    # kernel should override the `kernel` arg of
    # bpfman-e2e-targets-kmod from their NixOS module rather
    # than expecting this default to match.
    nixpkgs-kmod.url = "github:NixOS/nixpkgs/10e7ad5bbcb421fe07e3a4ad53a634b0cd57ffac";
  };

  outputs = { self, nixpkgs, nixpkgs-kmod }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system:
        f (import nixpkgs { inherit system; }));
      # Per-system pkgs from the kmod-pinned nixpkgs, used only
      # for the bpfman-e2e-targets-kmod derivation so its
      # kernel matches NixOS hosts tracking the same channel.
      kmodPkgsFor = system: import nixpkgs-kmod { inherit system; };
    in
    {
      packages = nixpkgs.lib.genAttrs systems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          kmodPkgs = kmodPkgsFor system;
        in rec {
        default = bpfman;
        # Statically linked against a scratch-compatible base; the
        # portable binary you can cp to any linux-x86_64 or
        # linux-aarch64 host regardless of its glibc version.
        bpfman = pkgs.callPackage ./nix/package.nix {
          inherit self;
          static = true;
        };
        # Nix-native dynamic build: lighter, quicker link, but the
        # produced binary's interpreter points into this Nix store
        # so it only runs on hosts that can resolve it.
        bpfman-dynamic = pkgs.callPackage ./nix/package.nix {
          inherit self;
          static = false;
        };
        # Pure-Nix OCI image: the static `bpfman` plus a small
        # debug toolkit (bash, coreutils, bpftool, iproute2, procps,
        # strace), built without a Docker daemon. See nix/image.nix
        # for rationale; `make build-image-nix` for the
        # build-and-load convenience target.
        bpfman-image = pkgs.callPackage ./nix/image.nix {
          inherit bpfman;
        };
        # Out-of-tree kernel module exporting private fentry/fexit
        # targets for the e2e suite. Built against the kmod-pinned
        # nixpkgs (nixos-25.11) rather than the unstable channel,
        # so its kernel headers match NixOS hosts on that channel
        # for vermagic compatibility. On a host running a different
        # kernel, override `kernel` from your NixOS module to align:
        #
        #   bpfman-e2e-targets-kmod.override {
        #     kernel = config.boot.kernelPackages.kernel;
        #   }
        #
        # See docs/HERMETIC-FENTRY-FEXIT-KMOD.md for context.
        bpfman-e2e-targets-kmod = kmodPkgs.callPackage ./nix/kmod.nix {
          kernel = kmodPkgs.linuxPackages.kernel;
        };
      });


      devShells = forAllSystems (pkgs: rec {
        default = pkgs.mkShell {
          packages = with pkgs; [
            # Go toolchain and CGO.
            gcc
            git
            gnumake
            go_1_25
            pkg-config

            # BPF build toolchain. Use clang-unwrapped: the cc-
            # wrapper warns "supplying --target bpfel != x86_64-
            # unknown-linux-gnu may not work correctly" because it
            # injects host-target -isystem (glibc-dev, ncurses,
            # zlib, ...), --gcc-toolchain, NIX_LDFLAGS, and
            # hardening flags (-fzero-call-used-regs, -fstack-
            # protector-strong) that clang either rejects or
            # ignores for bpfel. The unwrapped clang has none of
            # that. We supply the only header set BPF actually
            # needs from system paths (linuxHeaders) via CPATH in
            # the shellHook below; libbpf headers come via
            # pkg-config --cflags libbpf in the BPF Makefiles.
            bpftools
            llvmPackages.clang-unwrapped
            libbpf
            linuxHeaders
            llvm

            # Proto/gRPC codegen (make bpfman-proto).
            protobuf
            protoc-gen-go
            protoc-gen-go-grpc

            # Lint, coverage, misc.
            checkmake
            golangci-lint
            hadolint
            iproute2
            jq
            shellcheck

            # Load generation for chasing async-teardown lag in
            # the e2e suite. Use alongside `bin/e2e.test
            # -test.count N` to surface the timing races that
            # arm64 CI hits naturally on slower runners.
            stress-ng

            # SQLite (CLI for inspection; dev headers for -tags cgo_sqlite).
            sqlite
            sqlite.dev
          ];

          shellHook = ''
            export CGO_ENABLED=1
            # Linker chain in this dev shell. `go build`'s
            # final link is:
            #
            #   cmd/link (external) -> $CC -> gcc-wrapper -> ld
            #
            # Every binary in that chain comes from /nix/store;
            # /usr/bin/ld and /usr/lib/gcc never participate.
            # cmd/link only takes this path because
            # GO_EXTLINK_ENABLED=1 (below) forces external
            # linkmode -- nixpkgs's compiled-in default is
            # =0, which would keep the link inside cmd/link
            # and never subprocess out at all. $CC and $CXX
            # come from stdenv's cc-wrapper setup-hook, which
            # runs before this shellHook.
            #
            # Override nixpkgs Go's compiled-in GO_EXTLINK_ENABLED=0
            # default, which pins cmd/link to internal linkmode
            # whenever the user does not pass -linkmode explicitly.
            # That breaks `make test STATIC=1` from the .#static
            # shell: the race runtime's syso (runtime/race/internal/
            # amd64v1) is in cmd/link's internal-OK allowlist, so
            # auto-mode picks internal -- but with glibc.static on
            # the .#static shell's link path, ld pulls libc.a's
            # archive members for __errno_location, getuid,
            # pthread_self, etc., and the internal linker cannot
            # relocate them ("relocation target X not defined").
            # osusergo,netgo do not save us here -- those drop the
            # cgo NSS resolvers from Go's net and os/user, but the
            # race runtime itself references general libc symbols.
            # Setting this to 1 lets cmd/link auto-detect external
            # mode (which then defers libc resolution to the system
            # linker, which handles it correctly).
            #
            # Trap to remember: GO_EXTLINK_ENABLED is *not* part of
            # Go's build-cache key. Removing this export and re-
            # running `make test` against a warm cache is a
            # cache-hit on every prebuilt link result and looks
            # like a clean build; the regression only surfaces on a
            # cold cache (CI, fresh clone, or `rm -rf .cache`).
            # Treat any "this turned out to be redundant" claim
            # about this variable with extreme suspicion.
            export GO_EXTLINK_ENABLED=1
            # `nix develop --ignore-env` (--pure) strips HOME, which
            # Go uses to locate ~/.cache/go-build and ~/go/pkg/mod
            # (and which `~`-using tools like git also need). When
            # HOME is absent, give it a per-user /tmp fallback so
            # caches still land somewhere writable. In normal
            # interactive use direnv inherits HOME from the user's
            # shell, the conditional is a no-op, and Go uses the
            # standard locations -- no `.cache/` polluting the
            # checkout.
            if [ -z "''${HOME:-}" ]; then
              export HOME="/tmp/nix-shell-home-$UID"
              mkdir -p "$HOME"
            fi
            export TMPDIR="''${TMPDIR:-/tmp}"
            # CPATH supplies linuxHeaders to the unwrapped clang
            # invocations done by the BPF Makefiles. clang reads
            # CPATH like gcc does, treating each entry as an
            # additional system-include directory. libbpf is
            # picked up separately via `pkg-config --cflags
            # libbpf`. Without this, unwrapped clang would fall
            # through to /usr/include/linux on a non-NixOS host
            # and silently use the host kernel headers.
            export CPATH="${pkgs.linuxHeaders}/include''${CPATH:+:}''${CPATH:-}"
          '';
        };

        # Opt-in shell for `make STATIC=1`. glibc.static supplies
        # libc.a and libpthread.a, but its `-L` entry contains only
        # archives — placing it in the default shell makes ld pick
        # libc.a over libc.so for ordinary dynamic builds and emit
        # glibc's NSS dlopen-at-runtime warnings. Keeping it isolated
        # here means `nix develop` is the warning-free everyday path
        # and `nix develop .#static` is the explicit static-link
        # entry point.
        static = default.overrideAttrs (old: {
          buildInputs = (old.buildInputs or [ ]) ++ [ pkgs.glibc.static ];
          shellHook = (old.shellHook or "") + ''
            export STATIC=1
          '';
        });
      });
    };
}
