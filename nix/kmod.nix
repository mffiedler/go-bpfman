# Out-of-tree build of bpfman_e2e_targets.ko against a chosen
# kernel. Parameterised on `kernel` so callers can build against
# the same kernel their NixOS host is booted with -- in a NixOS
# module, pass `boot.kernelPackages.kernel`; for ad-hoc flake
# builds, the flake passes pkgs.linuxPackages.kernel.
#
# The resulting derivation has the standard NixOS extra-modules
# layout, so it can drop into `boot.extraModulePackages` directly:
#
#   boot.extraModulePackages = [
#     (inputs.bpfman.packages.${system}.bpfman-e2e-targets-kmod.override {
#       kernel = config.boot.kernelPackages.kernel;
#     })
#   ];
{ stdenv
, kernel
, lib
, ...
}:

stdenv.mkDerivation {
  pname = "bpfman-e2e-targets-kmod";
  version = "0.1-${kernel.modDirVersion}";

  # Source is just the kbuild leaf -- no need to drag the whole
  # repo into the derivation.
  src = ../e2e/kmod;

  nativeBuildInputs = kernel.moduleBuildDependencies;

  # The kmod has no hardening-flag concerns; the kernel build
  # system supplies its own. Disabling these prevents the
  # nixpkgs cc-wrapper from injecting userspace flags that the
  # kernel Makefile rejects.
  hardeningDisable = [ "format" "pic" "stackprotector" "fortify" ];

  # The kernel's scripts/Makefile.modfinal step that generates
  # per-module BTF runs `[ -f vmlinux ]` against KDIR's CWD.
  # The store kernel.dev does have vmlinux at its top level
  # but kbuild looks for it inside lib/modules/<ver>/build,
  # which it isn't. Without it pahole prints
  # "Skipping BTF generation ... due to unavailability of
  # vmlinux" and the loaded module has no BTF -- so BPF
  # fentry/fexit cannot resolve our targets.
  #
  # Build a writable symlink-mirror of the kernel build tree
  # and drop a vmlinux symlink at its root, then use that as
  # KDIR. cp -rs makes per-file symlinks rather than copies,
  # so the cost is bookkeeping only.
  preBuild = ''
    cp -rs ${kernel.dev}/lib/modules/${kernel.modDirVersion}/build kbuild
    chmod -R u+w kbuild
    ln -sf ${kernel.dev}/vmlinux kbuild/vmlinux
  '';

  makeFlags = [
    "KDIR=$(PWD)/kbuild"
  ];

  installPhase = ''
    runHook preInstall
    mkdir -p $out/lib/modules/${kernel.modDirVersion}/extra
    cp bpfman_e2e_targets.ko $out/lib/modules/${kernel.modDirVersion}/extra/
    runHook postInstall
  '';

  meta = with lib; {
    description = "Private fentry/fexit targets for bpfman e2e tests";
    license = licenses.gpl2Only;
    platforms = platforms.linux;
  };
}
