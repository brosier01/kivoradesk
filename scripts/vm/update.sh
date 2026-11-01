#!/usr/bin/env bash
# Update the VM ("update"): back up the data disk, upgrade the Alpine
# packages, redeploy KivoraDesk from this checkout, then run the diagnostics.
# Files, browser profile and logins are kept.
#
# usage: update.sh [--no-backup] [--reboot]
#   --reboot  restart the VM when a new kernel was installed
set -euo pipefail
source "$(dirname "$0")/common.sh"
here=$(dirname "$0")

backup=1 reboot=0
for arg in "$@"; do
	case $arg in
	--no-backup) backup=0 ;;
	--reboot) reboot=1 ;;
	*) die "usage: update.sh [--no-backup] [--reboot]" ;;
	esac
done

((backup)) && "$here/backup.sh"

# Apply the network filter of this checkout.
define_nwfilter

log "upgrading Alpine packages"
"$here/ssh.sh" 'doas apk update -q && doas apk upgrade -q --available'
"$here/provision.sh"

# Alpine removes the running kernel's modules when it installs a new kernel:
# the VM must restart before it needs to load a module.
if ! "$here/ssh.sh" '[ -d "/lib/modules/$(uname -r)" ]'; then
	if ((reboot)); then
		log "new kernel installed: restarting the VM"
		virsh_ reboot "$VM_NAME" --mode agent >/dev/null
		sleep 10
		mapfile -t ssh_opts < <(vm_ssh_opts)
		for _ in $(seq 60); do
			ssh "${ssh_opts[@]}" "$VM_ADMIN@$VM_IP" true 2>/dev/null && break
			sleep 3
		done
		sleep 5 # let the services start
	else
		log "warning: a new kernel is installed; restart the VM soon (update.sh --reboot, or virsh reboot --mode agent $VM_NAME)"
	fi
fi

"$here/doctor.sh"
