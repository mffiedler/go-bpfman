{ lib
, stdenv
, clang
, gnumake
, libbpf
, linuxHeaders
, pkg-config
}:

# Shared BPF object derivation: produces the dispatcher and e2e
# testdata .bpf.o files that the Go tree embeds via go:embed. The
# output path lays them out under the same directory names the Go
# source expects, so consumers can `cp $out/dispatcher/*.bpf.o
# dispatcher/` (and the equivalent for e2e/testdata/bpf) in their
# own buildPhase.

stdenv.mkDerivation {
  pname = "bpfman-bpf-objects";
  version = "dev";

  src = ./..;

  nativeBuildInputs = [
    clang
    gnumake
    pkg-config
  ];

  buildInputs = [
    libbpf
    linuxHeaders
  ];

  # Clang rejects the cc-wrapper's default hardening flags
  # (-fstack-protector-strong, -fzero-call-used-regs=used-gpr, ...)
  # when targeting bpfel. Scope the override to just this derivation
  # so the rest of the flake (CGO compiles, dev shell) keeps hardening.
  hardeningDisable = [ "all" ];

  # The outputs are BPF bytecode, not ELF executables; skip the
  # fixup pass that would otherwise emit noisy `patchelf: wrong ELF
  # type` lines for every .bpf.o.
  dontPatchELF = true;

  buildPhase = ''
    runHook preBuild
    make -C dispatcher
    make -C e2e/testdata/bpf
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p $out/dispatcher $out/e2e/testdata/bpf
    cp dispatcher/*.bpf.o $out/dispatcher/
    cp e2e/testdata/bpf/*.bpf.o $out/e2e/testdata/bpf/
    runHook postInstall
  '';

  meta = {
    description = "Compiled eBPF objects for bpfman's dispatchers and e2e tests";
    license = lib.licenses.asl20;
    platforms = lib.platforms.linux;
  };
}
