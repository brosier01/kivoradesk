#!/usr/bin/env bash
# Remove the KivoraDesk VM and its volumes. The storage pool is kept.
set -euo pipefail
source "$(dirname "$0")/common.sh"

if [[ ${1:-} != --yes ]]; then
	die "this deletes VM '$VM_NAME' and its disk; re-run with --yes"
fi

virsh_ destroy "$VM_NAME" >/dev/null 2>&1 || true
virsh_ undefine "$VM_NAME" --nvram >/dev/null 2>&1 || virsh_ undefine "$VM_NAME" >/dev/null 2>&1 || true
for vol in "$VM_NAME.qcow2" "$VM_NAME-seed.iso"; do
	virsh_ vol-delete --pool "$LIBVIRT_POOL" "$vol" >/dev/null 2>&1 || true
done
log "VM $VM_NAME removed"
