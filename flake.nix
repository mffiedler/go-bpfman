{
  description = "go-bpfman development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system:
        f (import nixpkgs { inherit system; }));
    in
    {
      packages = forAllSystems (pkgs: rec {
        default = bpfman;
        # Compiled eBPF objects (dispatcher + e2e testdata). Exposed
        # as a package in its own right so CI can seed the source
        # tree without rebuilding, and so `packages.bpfman` can
        # consume it as a build input.
        bpf-objects = pkgs.callPackage ./nix/bpf-objects.nix { };
        # Statically linked against a scratch-compatible base; the
        # portable binary you can cp to any linux-x86_64 or
        # linux-aarch64 host regardless of its glibc version.
        bpfman = pkgs.callPackage ./nix/package.nix {
          inherit self bpf-objects;
          static = true;
        };
        # Nix-native dynamic build: lighter, quicker link, but the
        # produced binary's interpreter points into this Nix store
        # so it only runs on hosts that can resolve it.
        bpfman-dynamic = pkgs.callPackage ./nix/package.nix {
          inherit self bpf-objects;
          static = false;
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

            # BPF build toolchain.
            bpftools
            clang
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

            # SQLite (CLI for inspection; dev headers for -tags cgo_sqlite).
            sqlite
            sqlite.dev
          ];

          shellHook = ''
            export CGO_ENABLED=1
            # Nixpkgs builds Go with GO_EXTLINK_ENABLED=0 baked in as
            # the linker's compiled-in default, which forces internal
            # linkmode whenever the user does not pass -linkmode
            # explicitly. That breaks `go test -race`: the race
            # runtime's syso pulls in libc symbols (getaddrinfo,
            # __errno_location, pthread_*, ...) that the internal
            # linker cannot resolve dynamically, so the link fails
            # with "relocation target X not defined". Restoring the
            # upstream default (1 = auto) lets the linker pick
            # external mode when cgo/race host objects are present.
            export GO_EXTLINK_ENABLED=1
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
        });
      });
    };
}
