#!/usr/bin/env bash
# Check that this machine can host the KivoraDesk VM. create.sh runs it first.
# Exits non-zero if something is missing.
set -uo pipefail
source "$(dirname "$0")/common.sh"

failed=0
ok() { printf 'ok    %s\n' "$*"; }
warn() { printf 'WARN  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failed=1; }

if [[ $(uname -s) == Linux && $(uname -m) == x86_64 ]]; then
	ok "Linux x86_64"
else
	fail "$(uname -sm): the VM needs Linux on x86_64"
fi

if [[ -e /dev/kvm ]]; then
	ok "KVM"
else
	fail "no /dev/kvm: enable virtualization in the firmware, or use a cloud server that exposes it"
fi

missing=()
for cmd in virsh virt-install qemu-img xorriso gpg gpgv curl ssh ssh-keygen go; do
	command -v "$cmd" >/dev/null || missing+=("$cmd")
done
if ((${#missing[@]})); then
	fail "missing commands: ${missing[*]} (see Install in the README)"
else
	ok "commands"
fi

# Go fetches the toolchain go.mod asks for since 1.21.
if command -v go >/dev/null; then
	go_version=$(GOTOOLCHAIN=local go version | awk '{ print $3 }')
	if printf '%s\n' go1.21 "$go_version" | sort -V -C; then
		ok "Go $go_version"
	else
		fail "Go $go_version: 1.21 or later is needed (https://go.dev/dl/)"
	fi
fi

if ! virsh_ uri >/dev/null 2>&1; then
	fail "cannot reach libvirt at $LIBVIRT_URI: is it running, and are you in the libvirt group? (sudo usermod -aG libvirt \"\$USER\", then log in again)"
else
	ok "libvirt ($LIBVIRT_URI)"
	gateway=$(network_gateway)
	if [[ -n $gateway ]]; then
		ok "network $LIBVIRT_NETWORK ($gateway), VM address $VM_IP"
	else
		fail "libvirt has no network '$LIBVIRT_NETWORK' with an IPv4 address (LIBVIRT_NETWORK picks another)"
	fi
	if [[ $(virsh_ nwfilter-list) == *clean-traffic* ]]; then
		ok "libvirt network filters"
	else
		fail "libvirt has no clean-traffic filter, needed to isolate the VM (see Install in the README)"
	fi
fi

available=$(awk '/^MemAvailable:/ { print int($2 / 1024) }' /proc/meminfo)
if ((available >= VM_MEMORY_MB)); then
	ok "memory: $available MiB available"
else
	warn "memory: $available MiB available, the VM can use $VM_MEMORY_MB"
fi

needed=$((VM_DISK_GB + VM_DATA_GB))
free=$(df -BG --output=avail "$(dirname "$LIBVIRT_POOL_DIR")" 2>/dev/null | tail -n1 | tr -dc 0-9)
if [[ -n $free ]] && ((free < needed)); then
	warn "disk: $free GiB free, the VM's disks can grow to $needed GiB"
elif [[ -n $free ]]; then
	ok "disk: $free GiB free"
fi

exit $failed
