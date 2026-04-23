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
        # Statically linked against a scratch-compatible base; this
        # is the portable binary you can cp to any linux-x86_64 or
        # linux-aarch64 host irrespective of its glibc version.
        bpfman = pkgs.callPackage ./nix/package.nix {
          inherit self;
          static = true;
        };
        # Nix-native dynamic build: lighter, quicker link, but the
        # produced binary's interpreter path points into this Nix
        # store, so it only runs on hosts that can resolve it.
        bpfman-dynamic = pkgs.callPackage ./nix/package.nix {
          inherit self;
          static = false;
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            # Go toolchain and CGO. glibc.static supplies libc.a and
            # libpthread.a so `make STATIC=1` can link CGO binaries
            # against a scratch base.
            gcc
            git
            glibc.static
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
          '';
        };
      });
    };
}
