#!/usr/bin/env bash
# Back up the VM's data disk (/home/agent and /workspace: files, browser
# profile and logins) into an encrypted file, keeping the most recent ones.
#
# While the VM runs, libvirt takes a point-in-time copy: the guest filesystem
# is frozen only while the copy starts, not while it is written.
#
# usage: backup.sh                        back up now
#        backup.sh --install-cron "0 3 * * *"   back up on a schedule (crontab)
set -euo pipefail
source "$(dirname "$0")/common.sh"

if [[ ${1:-} == --install-cron ]]; then
	schedule=${2:?usage: backup.sh --install-cron "MIN HOUR DOM MON DOW"}
	line="$schedule $REPO_ROOT/scripts/vm/backup.sh >>$KIVORA_DATA_DIR/backup.log 2>&1 # kivoradesk-backup"
	{ crontab -l 2>/dev/null | grep -v '# kivoradesk-backup$' || true; echo "$line"; } | crontab -
	log "scheduled: $line"
	exit 0
fi
[[ $# -eq 0 ]] || die "usage: backup.sh [--install-cron SCHEDULE]"
command -v gpg >/dev/null || die "gpg is required to encrypt backups"

if [[ ! -f $KIVORA_BACKUP_PASSPHRASE ]]; then
	(umask 077 && mkdir -p "$(dirname "$KIVORA_BACKUP_PASSPHRASE")" &&
		head -c 32 /dev/urandom | base64 >"$KIVORA_BACKUP_PASSPHRASE")
	log "created $KIVORA_BACKUP_PASSPHRASE: keep a copy elsewhere, backups cannot be restored without it"
fi
mkdir -p "$KIVORA_BACKUP_DIR"
chmod 700 "$KIVORA_BACKUP_DIR"

data_vol=$VM_NAME-data.qcow2
copy_vol=$VM_NAME-data.backup.qcow2
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out=$KIVORA_BACKUP_DIR/$VM_NAME-data-$stamp.qcow2.gpg

# The copy of the disk is in clear: never leave it behind, even if a step fails.
work="" frozen=0
cleanup() {
	((frozen)) && virsh_ domfsthaw "$VM_NAME" >/dev/null 2>&1
	[[ -n $work ]] && rm -rf "$work"
	rm -f "$out.part"
	virsh_ vol-delete --pool "$LIBVIRT_POOL" "$copy_vol" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A copy left by an interrupted run.
virsh_ pool-refresh "$LIBVIRT_POOL" >/dev/null
virsh_ vol-delete --pool "$LIBVIRT_POOL" "$copy_vol" >/dev/null 2>&1 || true

if [[ $(virsh_ domstate "$VM_NAME") == running ]]; then
	work=$(mktemp -d)

	cat >"$work/backup.xml" <<EOF
<domainbackup mode='push'>
  <disks>
    <disk name='vdb' backup='yes' type='file'>
      <driver type='qcow2'/>
      <target file='$LIBVIRT_POOL_DIR/$copy_vol'/>
    </disk>
    <disk name='vda' backup='no'/>
    <disk name='sda' backup='no'/>
  </disks>
</domainbackup>
EOF
	if virsh_ domfsfreeze "$VM_NAME" --mountpoint /data >/dev/null 2>&1; then
		frozen=1
	else
		log "warning: could not freeze /data (guest agent down?); the copy is crash-consistent"
	fi
	virsh_ backup-begin "$VM_NAME" --backupxml "$work/backup.xml" >/dev/null
	if ((frozen)); then
		virsh_ domfsthaw "$VM_NAME" >/dev/null
		frozen=0
	fi

	# Capture virsh's output before matching: with pipefail, grep -q closing
	# the pipe early would turn a match into a failure.
	log "copying the data disk"
	while [[ $(virsh_ domjobinfo "$VM_NAME") == *Unbounded* ]]; do
		sleep 1
	done
	result=$(virsh_ domjobinfo "$VM_NAME" --completed --keep-completed)
	[[ $result == *Completed* ]] || die "backup job failed: $result"
	virsh_ pool-refresh "$LIBVIRT_POOL" >/dev/null
	source_vol=$copy_vol
else
	log "VM is not running: copying the data disk directly"
	source_vol=$data_vol
fi

log "encrypting into $out"
virsh_ vol-download --pool "$LIBVIRT_POOL" "$source_vol" /dev/stdout |
	gpg --batch --yes --quiet --pinentry-mode loopback --passphrase-file "$KIVORA_BACKUP_PASSPHRASE" \
		--symmetric --cipher-algo AES256 --compress-level 1 -o "$out.part"
mv "$out.part" "$out"
chmod 600 "$out"

# Keep the most recent backups only.
mapfile -t old < <(ls -1t "$KIVORA_BACKUP_DIR/$VM_NAME"-data-*.qcow2.gpg | tail -n +$((KIVORA_BACKUP_KEEP + 1)))
((${#old[@]})) && rm -f -- "${old[@]}" && log "removed ${#old[@]} old backup(s)"

log "backup done: $out ($(du -h "$out" | cut -f1))"
