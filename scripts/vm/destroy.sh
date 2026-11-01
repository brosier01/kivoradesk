#!/usr/bin/env bash
# Remove the KivoraDesk VM. With --keep-data, the data disk (/home/agent and
# /workspace) is kept so that scripts/vm/create.sh can rebuild around it.
set -euo pipefail
source "$(dirname "$0")/common.sh"

confirm=0 keep_data=0
for arg in "$@"; do
	case $arg in
	--yes) confirm=1 ;;
	--keep-data) keep_data=1 ;;
	*) die "unknown option $arg (usage: destroy.sh --yes [--keep-data])" ;;
	esac
done
if ((!confirm)); then
	die "this deletes VM '$VM_NAME' and its disks; re-run with --yes (add --keep-data to keep the data disk)"
fi

virsh_ destroy "$VM_NAME" >/dev/null 2>&1 || true
virsh_ undefine "$VM_NAME" --nvram >/dev/null 2>&1 || virsh_ undefine "$VM_NAME" >/dev/null 2>&1 || true

vols=("$VM_NAME.qcow2" "$VM_NAME-seed.iso")
((keep_data)) || vols+=("$VM_NAME-data.qcow2")
for vol in "${vols[@]}"; do
	virsh_ vol-delete --pool "$LIBVIRT_POOL" "$vol" >/dev/null 2>&1 || true
done
virsh_ net-update "$LIBVIRT_NETWORK" delete ip-dhcp-host "$(dhcp_host_xml)" --live --config >/dev/null 2>&1 || true
forget_host_key

if ((keep_data)); then
	log "VM $VM_NAME removed, data disk kept"
else
	log "VM $VM_NAME and its data removed"
fi
