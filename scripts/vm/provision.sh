#!/usr/bin/env bash
# Copy image/alpine to the VM and run the guest provisioning script.
set -euo pipefail
source "$(dirname "$0")/common.sh"

ip=$(vm_ip)
[[ -n $ip ]] || die "no IP address found for VM '$VM_NAME' (is it running?)"
mapfile -t ssh_opts < <(vm_ssh_opts)

log "provisioning $VM_NAME ($ip)"
tar -C "$REPO_ROOT/image/alpine" -cf - . |
	ssh "${ssh_opts[@]}" "$VM_ADMIN@$ip" '
		set -e
		dir=$(mktemp -d)
		trap "doas rm -rf $dir" EXIT
		tar -C "$dir" -xf -
		cd "$dir" && doas sh ./provision.sh
	'
