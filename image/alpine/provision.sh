#!/bin/sh
# Provision an Alpine guest as a KivoraDesk desktop. Runs as root, idempotent.
# Expects to be run from the directory that contains rootfs/.
set -eu

AGENT_USER=${AGENT_USER:-agent}
# Fixed UID: files on the data disk must keep their owner when the system
# disk is rebuilt.
AGENT_UID=${AGENT_UID:-1100}
# The worker runs the agent's shell commands and file operations, without
# access to the display, the browser or the control API.
WORKER_USER=${WORKER_USER:-worker}
WORKER_UID=${WORKER_UID:-1101}
# kivora-ws shares /workspace between agent and worker (fixed GID: the ACLs
# live on the data disk); kivora-ctl may use the control API.
WS_GROUP=kivora-ws WS_GID=1200
CTL_GROUP=kivora-ctl CTL_GID=1201
WEB_USER=${WEB_USER:-kivora-web}
WORKSPACE=${WORKSPACE:-/workspace}
DATA_DIR=/data
DATA_SERIAL=kivora-data
SERVICES="kivora-firewall kivora-display kivora-wm kivora-browser kivora-vnc kivora-web"

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
	chromium font-dejavu dbus \
	bash curl libgcc libstdc++ ripgrep xclip \
	e2fsprogs acl nftables qemu-guest-agent

# Only noVNC's web client is needed: the package also pulls websockify,
# Python and NumPy, which the kivora-web gateway replaces.
log "installing the noVNC web client"
apk info -e novnc >/dev/null && apk del -q novnc
novnc_tmp=$(mktemp -d)
(cd "$novnc_tmp" && apk fetch -q novnc && mkdir root && apk extract -q --destination root novnc-*.apk >/dev/null)
rm -rf /usr/share/novnc
mv "$novnc_tmp/root/usr/share/novnc" /usr/share/novnc
rm -rf "$novnc_tmp"

# cloud-init only matters on the first boot, which created the admin
# account, the SSH keys and the network configuration. Afterwards it slows
# every boot down and keeps Python installed.
if apk info -e cloud-init >/dev/null; then
	log "removing cloud-init"
	cloud-init status --wait >/dev/null 2>&1 || true # first boot finished
	apk del -q cloud-init
fi
for svc in cloud-final cloud-config cloud-init cloud-init-hotplugd cloud-init-local cloud-getty; do
	rc-update -a del "$svc" >/dev/null 2>&1 || true
done

# The VM's address is reserved for its MAC on the libvirt network: skip the
# ARP probe for conflicts, which holds every service back for 5 s at boot.
grep -qx noarp /etc/dhcpcd.conf 2>/dev/null || echo noarp >>/etc/dhcpcd.conf

# Empty /tmp at boot: Chromium leaves directories there when it crashes.
sed -i 's/^wipe_tmp=.*/wipe_tmp="YES"/' /etc/conf.d/bootmisc

# Boot without waiting at the boot menu.
[ -f /boot/limine/limine.conf ] && sed -i 's/^timeout: .*/timeout: 1/' /boot/limine/limine.conf

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

if ! id "$WORKER_USER" >/dev/null 2>&1; then
	log "creating unprivileged user $WORKER_USER"
	adduser -D -u "$WORKER_UID" -s /bin/sh -g "KivoraDesk worker" "$WORKER_USER"
fi

getent group "$WS_GROUP" >/dev/null || addgroup -g "$WS_GID" "$WS_GROUP"
getent group "$CTL_GROUP" >/dev/null || addgroup -g "$CTL_GID" "$CTL_GROUP"
for member in "$AGENT_USER:$WS_GROUP" "$WORKER_USER:$WS_GROUP" "$AGENT_USER:$CTL_GROUP"; do
	addgroup "${member%%:*}" "${member#*:}" 2>/dev/null || true
done

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
	bind_to_data "/home/$WORKER_USER"
	bind_to_data "$WORKSPACE"
else
	log "no data disk (serial $DATA_SERIAL): keeping data on the system disk"
fi

# A data disk from an older VM may carry other UIDs.
for entry in "$AGENT_USER:/home/$AGENT_USER" "$AGENT_USER:$WORKSPACE" "$WORKER_USER:/home/$WORKER_USER"; do
	user=${entry%%:*} dir=${entry#*:}
	if [ -d "$dir" ] && [ "$(stat -c %u "$dir")" != "$(id -u "$user")" ]; then
		log "fixing ownership of $dir"
		chown -R "$user:$user" "$dir"
	fi
done
chmod 0700 "/home/$AGENT_USER" "/home/$WORKER_USER"

log "preparing $WORKSPACE"
mkdir -p "$WORKSPACE"
chown "$AGENT_USER:$WS_GROUP" "$WORKSPACE"
chmod 2770 "$WORKSPACE"
# Default ACLs give the shared group read/write on everything created inside,
# whatever the creating process's umask.
setfacl -R -m "g:$WS_GROUP:rwX" -m "d:u::rwX,d:g::rwX,d:g:$WS_GROUP:rwX,d:o::-" "$WORKSPACE"

log "installing services"
cp -R rootfs/. /
chmod 0755 /etc/init.d/kivora-* /usr/local/bin/kivora-*
chmod 0400 /etc/doas.d/*.conf
doas -C /etc/doas.d/kivora-mcp.conf
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
