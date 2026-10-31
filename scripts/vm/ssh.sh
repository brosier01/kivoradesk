#!/usr/bin/env bash
# Open an SSH session (or run a command) on the KivoraDesk VM as the admin user.
set -euo pipefail
source "$(dirname "$0")/common.sh"

ip=$(vm_ip)
[[ -n $ip ]] || die "no IP address found for VM '$VM_NAME' (is it running?)"
mapfile -t ssh_opts < <(vm_ssh_opts)
exec ssh "${ssh_opts[@]}" "$VM_ADMIN@$ip" "$@"
