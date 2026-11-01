#!/usr/bin/env bash
# Replace the VM's data disk with a backup ("reset"). The current data is
# backed up first, so a restore can itself be undone.
#
# usage: restore.sh --yes [BACKUP_FILE]   (default: the most recent backup)
#        restore.sh --list
set -euo pipefail
source "$(dirname "$0")/common.sh"

if [[ ${1:-} == --list ]]; then
	ls -1t "$KIVORA_BACKUP_DIR/$VM_NAME"-data-*.qcow2.gpg 2>/dev/null || log "no backup in $KIVORA_BACKUP_DIR"
	exit 0
fi
[[ ${1:-} == --yes ]] || die "this replaces /home/agent and /workspace in the VM; re-run with --yes [BACKUP_FILE]"
file=${2:-$(ls -1t "$KIVORA_BACKUP_DIR/$VM_NAME"-data-*.qcow2.gpg 2>/dev/null | head -n1)}
[[ -f $file ]] || die "no backup to restore (looked in $KIVORA_BACKUP_DIR)"
[[ -f $KIVORA_BACKUP_PASSPHRASE ]] || die "passphrase file $KIVORA_BACKUP_PASSPHRASE is missing"
command -v qemu-img >/dev/null || die "qemu-img is required"

work=$(mktemp -d "${TMPDIR:-/tmp}/kivoradesk-restore.XXXXXX")
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"

log "decrypting $file"
gpg --batch --quiet --pinentry-mode loopback --passphrase-file "$KIVORA_BACKUP_PASSPHRASE" \
	-d -o "$work/data.qcow2" "$file"
qemu-img check -q "$work/data.qcow2" || die "the backup is not a valid disk image"

if [[ $(virsh_ domstate "$VM_NAME" 2>/dev/null) == running ]]; then
	log "backing up the current data first"
	"$(dirname "$0")/backup.sh"
	log "shutting the VM down"
	virsh_ shutdown "$VM_NAME" --mode agent >/dev/null
	for _ in $(seq 60); do
		[[ $(virsh_ domstate "$VM_NAME") == "shut off" ]] && break
		sleep 1
	done
	[[ $(virsh_ domstate "$VM_NAME") == "shut off" ]] || virsh_ destroy "$VM_NAME" >/dev/null
fi

log "replacing the data disk"
vol=$VM_NAME-data.qcow2
virsh_ vol-delete --pool "$LIBVIRT_POOL" "$vol" >/dev/null
virsh_ vol-create-as "$LIBVIRT_POOL" "$vol" "$(stat -c %s "$work/data.qcow2")" --format qcow2 >/dev/null
virsh_ vol-upload --pool "$LIBVIRT_POOL" "$vol" "$work/data.qcow2"

log "starting the VM"
virsh_ start "$VM_NAME" >/dev/null
mapfile -t ssh_opts < <(vm_ssh_opts)
for _ in $(seq 60); do
	ssh "${ssh_opts[@]}" "$VM_ADMIN@$VM_IP" true 2>/dev/null && break
	sleep 3
done
log "restored $file"
"$(dirname "$0")/doctor.sh" >/dev/null || log "warning: doctor.sh reports problems; run it for details"
