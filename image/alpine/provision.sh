#!/bin/sh
# Provision an Alpine guest as a KivoraDesk desktop. Runs as root, idempotent.
# Expects to be run from the directory that contains rootfs/.
set -eu

AGENT_USER=${AGENT_USER:-agent}
# Fixed UID: files on the data disk must keep their owner when the system
# disk is rebuilt.
AGENT_UID=${AGENT_UID:-1100}
WEB_USER=${WEB_USER:-kivora-web}
WORKSPACE=${WORKSPACE:-/workspace}
DATA_DIR=/data
DATA_SERIAL=kivora-data
SERVICES="kivora-display kivora-wm kivora-browser kivora-vnc kivora-web"

log() { printf '==> %s\n' "$*" >&2; }

# data_disk prints the block device whose virtio serial is $DATA_SERIAL.
data_disk() {
	for serial in /sys/block/vd*/serial; do
		[ -f "$serial" ] || continue
		if [ "$(cat "$serial")" = "$DATA_SERIAL" ]; then
			echo "/dev/$(basename "$(dirname "$serial")")"
			return
		fi
	done
}

# bind_to_data moves DIR onto the data disk (first time only) and bind-mounts
# it back in place.
bind_to_data() {
	dir=$1 target=$DATA_DIR$1
	mountpoint -q "$dir" && return
	if [ ! -d "$target" ]; then
		mkdir -p "$target"
		[ -d "$dir" ] && cp -a "$dir/." "$target/"
	fi
	mkdir -p "$dir"
	grep -q "^$target $dir " /etc/fstab || echo "$target $dir none bind 0 0" >>/etc/fstab
	mount "$dir"
}

log "installing packages"
apk update -q
apk add -q \
	xvfb xauth openbox xdotool x11vnc scrot xwininfo xprop \
	chromium font-dejavu dbus novnc \
	bash curl libgcc libstdc++ ripgrep \
	e2fsprogs qemu-guest-agent

# The Alpine virt image exposes no ACPI power button, so libvirt shuts the
# VM down and freezes filesystems for snapshots through the guest agent.
rc-update add qemu-guest-agent default >/dev/null
rc-service qemu-guest-agent start >/dev/null 2>&1 || true

if ! id "$AGENT_USER" >/dev/null 2>&1; then
	log "creating unprivileged user $AGENT_USER"
	adduser -D -u "$AGENT_UID" -s /bin/bash -g "KivoraDesk agent" "$AGENT_USER"
fi
# Alpine's sshd refuses accounts whose shadow entry starts with "!", keys
# included. "*" means no usable password without locking key logins.
sed -i "s/^$AGENT_USER:!:/$AGENT_USER:*:/" /etc/shadow

if ! id "$WEB_USER" >/dev/null 2>&1; then
	log "creating system user $WEB_USER"
	adduser -S -D -H -h /var/lib/$WEB_USER -s /sbin/nologin "$WEB_USER"
fi
install -d -m 700 -o "$WEB_USER" "/var/lib/$WEB_USER"

disk=$(data_disk)
if [ -n "$disk" ]; then
	log "setting up data disk $disk"
	if [ -z "$(blkid "$disk")" ]; then
		mkfs.ext4 -q -L "$DATA_SERIAL" "$disk"
	fi
	mkdir -p "$DATA_DIR"
	grep -q "^LABEL=$DATA_SERIAL " /etc/fstab ||
		echo "LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noatime 0 2" >>/etc/fstab
	mountpoint -q "$DATA_DIR" || mount "$DATA_DIR"
	if ! mountpoint -q "/home/$AGENT_USER"; then
		for svc in $SERVICES; do rc-service -q "$svc" stop 2>/dev/null || true; done
	fi
	bind_to_data "/home/$AGENT_USER"
	bind_to_data "$WORKSPACE"
else
	log "no data disk (serial $DATA_SERIAL): keeping data on the system disk"
fi

# A data disk from an older VM may carry another UID for the agent.
uid=$(id -u "$AGENT_USER")
for dir in "/home/$AGENT_USER" "$WORKSPACE"; do
	if [ -d "$dir" ] && [ "$(stat -c %u "$dir")" != "$uid" ]; then
		log "fixing ownership of $dir"
		chown -R "$AGENT_USER:$AGENT_USER" "$dir"
	fi
done

log "preparing $WORKSPACE"
mkdir -p "$WORKSPACE"
chown "$AGENT_USER:$AGENT_USER" "$WORKSPACE" "/home/$AGENT_USER"
chmod 0750 "$WORKSPACE"

log "installing services"
cp -R rootfs/. /
chmod 0755 /etc/init.d/kivora-*
rc-service sshd reload >/dev/null

# Stop in reverse order, then start in order: restarting the display alone
# would also restart its dependents and race with explicit restarts.
for svc in $(echo "$SERVICES" | tr ' ' '\n' | tac); do
	rc-service -q "$svc" stop 2>/dev/null || true
done
for svc in dbus $SERVICES; do
	rc-update add "$svc" default >/dev/null
	rc-service -q "$svc" start
done

log "provisioning done"
