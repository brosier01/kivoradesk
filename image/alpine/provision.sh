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
# The vault account alone reads the credentials, out of reach of the browser,
# which runs as the agent. They stay on the data disk.
VAULT_USER=${VAULT_USER:-kivora-vault}
VAULT_UID=${VAULT_UID:-1102}
VAULT_DIR=/var/lib/kivora-vault
# The agent's SSH keys, on the data disk but out of its reach.
KEYS_DIR=/etc/ssh/authorized_keys
# The audit log, on the data disk, out of the agent's reach.
AUDIT_USER=${AUDIT_USER:-kivora-audit}
AUDIT_UID=${AUDIT_UID:-1103}
AUDIT_DIR=/var/lib/kivora-audit
WORKSPACE=${WORKSPACE:-/workspace}
DATA_DIR=/data
DATA_SERIAL=kivora-data
SERVICES="kivora-hidepid kivora-firewall kivora-display kivora-wm kivora-browser kivora-vnc kivora-web"

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
# it back in place, without setuid programs or device files.
bind_to_data() {
	dir=$1 target=$DATA_DIR$1
	sed -i "s|^$target $dir none bind 0 0$|$target $dir none bind,noatime,nosuid,nodev 0 0|" /etc/fstab
	if ! mountpoint -q "$dir"; then
		if [ ! -d "$target" ]; then
			mkdir -p "$target"
			[ -d "$dir" ] && cp -a "$dir/." "$target/"
		fi
		mkdir -p "$dir"
		grep -q "^$target $dir " /etc/fstab || echo "$target $dir none bind,noatime,nosuid,nodev 0 0" >>/etc/fstab
		mount "$dir"
	fi
	# A bind mount only takes its options on a remount.
	mount -o remount,bind,noatime,nosuid,nodev "$dir"
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

if ! id "$VAULT_USER" >/dev/null 2>&1; then
	log "creating unprivileged user $VAULT_USER"
	adduser -D -H -u "$VAULT_UID" -h "$VAULT_DIR" -s /sbin/nologin -g "KivoraDesk credentials" "$VAULT_USER"
fi
if ! id "$AUDIT_USER" >/dev/null 2>&1; then
	log "creating unprivileged user $AUDIT_USER"
	adduser -D -H -u "$AUDIT_UID" -h "$AUDIT_DIR" -s /sbin/nologin -g "KivoraDesk audit log" "$AUDIT_USER"
fi
mkdir -p "$VAULT_DIR" "$KEYS_DIR" "$AUDIT_DIR"

disk=$(data_disk)
if [ -n "$disk" ]; then
	log "setting up data disk $disk"
	if [ -z "$(blkid "$disk")" ]; then
		mkfs.ext4 -q -L "$DATA_SERIAL" "$disk"
	fi
	mkdir -p "$DATA_DIR"
	sed -i "s|^LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noatime 0 2$|LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noatime,nosuid,nodev 0 2|" /etc/fstab
	grep -q "^LABEL=$DATA_SERIAL " /etc/fstab ||
		echo "LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noatime,nosuid,nodev 0 2" >>/etc/fstab
	mountpoint -q "$DATA_DIR" || mount "$DATA_DIR"
	mount -o remount,noatime,nosuid,nodev "$DATA_DIR"
	if ! mountpoint -q "/home/$AGENT_USER"; then
		for svc in $SERVICES; do rc-service -q "$svc" stop 2>/dev/null || true; done
	fi
	bind_to_data "/home/$AGENT_USER"
	bind_to_data "/home/$WORKER_USER"
	bind_to_data "$WORKSPACE"
	bind_to_data "$VAULT_DIR"
	bind_to_data "$KEYS_DIR"
	bind_to_data "$AUDIT_DIR"
else
	log "no data disk (serial $DATA_SERIAL): keeping data on the system disk"
fi

# A data disk from an older VM may carry other UIDs.
for entry in "$AGENT_USER:/home/$AGENT_USER" "$AGENT_USER:$WORKSPACE" "$WORKER_USER:/home/$WORKER_USER" "$VAULT_USER:$VAULT_DIR" "$AUDIT_USER:$AUDIT_DIR"; do
	user=${entry%%:*} dir=${entry#*:}
	if [ -d "$dir" ] && [ "$(stat -c %u "$dir")" != "$(id -u "$user")" ]; then
		log "fixing ownership of $dir"
		chown -Rh "$user:$user" "$dir"
	fi
done
chmod 0700 "/home/$AGENT_USER" "/home/$WORKER_USER" "$VAULT_DIR" "$AUDIT_DIR"

# The audit log used to live in the agent's home, where it could rewrite it.
old_audit=/home/$AGENT_USER/.local/state/kivora/audit
if [ -d "$old_audit" ]; then
	log "moving the audit log to the $AUDIT_USER account"
	if su -s /bin/sh "$AGENT_USER" -c "cd '$old_audit' && tar -cf - audit-*.jsonl" 2>/dev/null |
		su -s /bin/sh "$AUDIT_USER" -c "tar -C '$AUDIT_DIR' -xf -" 2>/dev/null; then
		su -s /bin/sh "$AGENT_USER" -c "rm -rf '$old_audit'"
	else
		log "warning: could not move $old_audit: left in place"
	fi
fi

# Credentials used to live in the agent's home, where the browser could read
# them. Each step runs as the account concerned: root must not follow a link
# planted in that home.
old_vault=/home/$AGENT_USER/.local/state/kivora/secrets.json
if [ -e "$old_vault" ] || [ -L "$old_vault" ]; then
	log "moving the credentials to the $VAULT_USER account"
	creds=$(su -s /bin/sh "$AGENT_USER" -c "cat '$old_vault'" 2>/dev/null) || creds=
	if [ -e "$VAULT_DIR/secrets.json" ]; then
		log "warning: the vault already holds credentials: deleting $old_vault without merging it"
	elif [ -n "$creds" ]; then
		printf '%s' "$creds" | su -s /bin/sh "$VAULT_USER" -c "umask 077; cat >'$VAULT_DIR/secrets.json'"
	fi
	unset creds
	su -s /bin/sh "$AGENT_USER" -c "rm -f '$old_vault'"
fi

# SSH keys used to live in the agent's home, where anything running as the
# agent could add one. As for the credentials, the old file is read as the agent.
chown root:root "$KEYS_DIR"
chmod 0755 "$KEYS_DIR"
old_keys=/home/$AGENT_USER/.ssh/authorized_keys
new_keys=$KEYS_DIR/$AGENT_USER
if [ -e "$old_keys" ] || [ -L "$old_keys" ]; then
	log "moving the SSH keys of $AGENT_USER to $KEYS_DIR"
	keys=$(su -s /bin/sh "$AGENT_USER" -c "cat '$old_keys'" 2>/dev/null) || keys=
	if [ -s "$new_keys" ]; then
		log "warning: $new_keys already holds keys: deleting $old_keys without merging it"
	elif [ -n "$keys" ]; then
		printf '%s\n' "$keys" >"$new_keys"
	fi
	unset keys
	su -s /bin/sh "$AGENT_USER" -c "rm -f '$old_keys'"
fi
[ -e "$new_keys" ] || : >"$new_keys"
chown "root:$AGENT_USER" "$new_keys"
chmod 0640 "$new_keys"

log "preparing $WORKSPACE"
mkdir -p "$WORKSPACE"
chown "$AGENT_USER:$WS_GROUP" "$WORKSPACE"
chmod 2770 "$WORKSPACE"
# Default ACLs give the shared group read/write on everything created inside,
# whatever the creating process's umask.
setfacl -RP -m "g:$WS_GROUP:rwX" -m "d:u::rwX,d:g::rwX,d:g:$WS_GROUP:rwX,d:o::-" "$WORKSPACE"

log "installing services"
cp -R rootfs/. /
chmod 0755 /etc/init.d/kivora-* /usr/local/bin/kivora-*
chmod 0400 /etc/doas.d/*.conf
chmod 0755 /etc/chromium/policies /etc/chromium/policies/managed
chmod 0644 /etc/chromium/policies/managed/*.json
chmod 0644 /etc/sysctl.d/kivoradesk.conf
sysctl -q -p /etc/sysctl.d/kivoradesk.conf
doas -C /etc/doas.d/kivora-mcp.conf
# OpenRC compares its dependency cache with the scripts to the second: a
# cache rebuilt in the second the scripts were copied looks current without
# them, is saved at shutdown and leaves the services stopped at the next
# boot. Rebuild it now.
rc-update -u >/dev/null
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
