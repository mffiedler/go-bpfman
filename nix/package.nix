{ lib
, stdenv
, bpf-objects
, glibc
, gnumake
, go_1_25
, self
, static ? true
}:

# Build bpfman by delegating to the repo's Makefile rather than
# duplicating its tag/ldflags logic. The Makefile already knows how
# to produce a static binary (`make STATIC=1 bpfman-compile`), so
# this derivation just provides the toolchain, captures version
# metadata from the flake's `self`, and installs the resulting
# binary into the Nix store.

stdenv.mkDerivation rec {
  pname = "bpfman";
  version = self.shortRev or "dirty";

  # ./.. resolves to the repo root because this file lives in nix/.
  src = ./..;

  nativeBuildInputs = [
    gnumake
    go_1_25
  ];

  # glibc.static supplies libc.a / libpthread.a so the CGO link step
  # can produce a static binary against a scratch-compatible base
  # when static=true. The BPF toolchain is intentionally absent: the
  # .bpf.o files come pre-built via the `bpf-objects` derivation and
  # get copied into place in buildPhase, so this derivation only
  # needs the Go toolchain and (when static) glibc's static archives.
  buildInputs = lib.optional static glibc.static;

  # Pass flake-captured git metadata through the Makefile's existing
  # ldflags pipeline. GIT_BRANCH is not meaningful for a flake-rev
  # build. BUILD_DATE is formatted from self.lastModifiedDate (the
  # flake input's commit time for clean builds, or the current wall
  # clock for dirty ones); the Makefile's `ifndef BUILD_DATE` only
  # runs `date` when BUILD_DATE is undefined, so an explicit value
  # via env wins. Deriving from the flake input rather than
  # $(shell date) keeps two clean builds of the same revision
  # byte-identical.
  env.GIT_COMMIT = self.rev or self.dirtyRev or "dirty";
  env.GIT_BRANCH = "";
  env.GIT_STATE = if self ? rev then "clean" else "dirty";
  env.GIT_VERSION = version;
  env.BUILD_DATE = let
    d = self.lastModifiedDate;  # "YYYYMMDDHHMMSS"
    y  = builtins.substring  0 4 d;
    mo = builtins.substring  4 2 d;
    dd = builtins.substring  6 2 d;
    hh = builtins.substring  8 2 d;
    mm = builtins.substring 10 2 d;
    ss = builtins.substring 12 2 d;
  in "${y}-${mo}-${dd}T${hh}:${mm}:${ss}Z";

  # Strip every Nix-sandbox path out of the binary. -trimpath maps
  # /build/<hash>-source -> the module path in DWARF source refs;
  # -s drops the symbol table, -w drops DWARF entirely. The two
  # together take the closure from ~30 paths to just the output
  # itself, and make the binary safe to cp to any linux host.
  env.EXTRA_GOFLAGS = "-trimpath";
  env.EXTRA_GO_LDFLAGS = "-s -w";

  # For a statically linked binary there is no .dynamic section to
  # rewrite, so Nix's default fixup pass prints a harmless
  # "cannot find section '.dynamic'" warning from patchelf. Skip
  # the ELF-patching stages entirely when static=true.
  dontPatchELF = static;

  buildPhase = ''
    runHook preBuild

    # Go's toolchain writes caches under $HOME, which is unwritable
    # in the sandbox by default. Redirect everything into $TMPDIR
    # and turn off module fetching; vendor/ is committed.
    export HOME=$TMPDIR
    export GOPATH=$TMPDIR/go
    export GOCACHE=$TMPDIR/go-cache
    export GOFLAGS=-mod=vendor

    # Seed the BPF objects from the shared derivation. The dispatcher
    # Go package embeds them via go:embed, so they must be present
    # before `go build` runs.
    cp ${bpf-objects}/dispatcher/*.bpf.o dispatcher/

    make bpfman-compile ${lib.optionalString static "STATIC=1"}

    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    install -Dm755 bin/bpfman $out/bin/bpfman
    runHook postInstall
  '';

  meta = {
    description = "Go reimplementation of bpfman";
    homepage = "https://github.com/frobware/go-bpfman";
    license = lib.licenses.asl20;
    mainProgram = "bpfman";
    platforms = lib.platforms.linux;
  };
}
