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
            # Go toolchain and CGO.
            go_1_25
            gcc
            pkg-config
            gnumake
            git

            # BPF build toolchain.
            clang
            llvm
            libbpf
            bpftools
            linuxHeaders

            # Proto/gRPC codegen (make bpfman-proto).
            protobuf
            protoc-gen-go
            protoc-gen-go-grpc

            # Lint, coverage, misc.
            golangci-lint
            jq
            iproute2

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
