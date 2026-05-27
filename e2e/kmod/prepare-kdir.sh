#!/usr/bin/env bash
set -euo pipefail

kernel_release=${KERNEL_RELEASE:-$(uname -r)}
kernel_mod_dir_version=${KERNEL_MOD_DIR_VERSION:-$kernel_release}
default_kdir="/lib/modules/${kernel_release}/build"
kdir=${KDIR:-$default_kdir}
kernel_dev=${KERNEL_DEV:-}
kbuild=${E2E_KMOD_KBUILD:-e2e/kmod/.kbuild}
if [[ "$kbuild" != /* ]]; then
	kbuild="$(pwd -P)/$kbuild"
fi

find_nixos_kernel_dev() {
	local kernel_image kernel_out kernel_drv output

	command -v nix-store >/dev/null 2>&1 || return 1
	[[ -e /run/current-system/kernel ]] || return 1

	kernel_image=$(readlink -f /run/current-system/kernel) || return 1
	kernel_out=${kernel_image%/bzImage}
	[[ -e "$kernel_out" ]] || return 1

	kernel_drv=$(nix-store -q --deriver "$kernel_out" 2>/dev/null) || return 1
	[[ "$kernel_drv" == *.drv ]] || return 1

	while IFS= read -r output; do
		if [[ "$output" == *-linux-"${kernel_release}"-dev ]]; then
			if [[ ! -e "$output" ]]; then
				nix-store -r "$output" >/dev/null
			fi
			printf '%s\n' "$output"
			return 0
		fi
	done < <(nix-store -q --outputs "$kernel_drv" 2>/dev/null)

	return 1
}

prepare_kernel_dev() {
	local kernel_build=$1

	if [[ ! -d "$kernel_build" ]]; then
		{
			echo "error: kernel build tree not found: $kernel_build"
			echo "Set KERNEL_MOD_DIR_VERSION=... if the module directory version differs from uname -r."
		} >&2
		exit 1
	fi

	if [[ -d "$kbuild" ]]; then
		find "$kbuild" -type d -exec chmod u+w {} +
		rm -rf "$kbuild"
	fi
	mkdir -p "$kbuild"
	cp -rs "$kernel_build"/. "$kbuild"/
	find "$kbuild" -type d -exec chmod u+w {} +

	if [[ -e "${kernel_dev}/vmlinux" ]]; then
		ln -sf "${kernel_dev}/vmlinux" "$kbuild/vmlinux"
	elif [[ -e /sys/kernel/btf/vmlinux ]]; then
		ln -sf /sys/kernel/btf/vmlinux "$kbuild/vmlinux"
	fi

	printf '%s\n' "$kbuild"
}

if [[ -d "$kdir" ]]; then
	printf '%s\n' "$kdir"
	exit 0
fi

if [[ -n "$kernel_dev" ]]; then
	prepare_kernel_dev "${kernel_dev}/lib/modules/${kernel_mod_dir_version}/build"
	exit 0
fi

if kernel_dev=$(find_nixos_kernel_dev); then
	kernel_mod_dir_version=$kernel_release
	prepare_kernel_dev "${kernel_dev}/lib/modules/${kernel_mod_dir_version}/build"
	exit 0
fi

{
	echo "error: KDIR=$kdir does not exist"
	echo "Install matching kernel headers/build tree or pass KDIR=..."
	echo "On NixOS, make sure the current kernel derivation is still available, or pass KERNEL_DEV=..."
} >&2
exit 1
