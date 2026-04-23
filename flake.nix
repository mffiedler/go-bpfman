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
