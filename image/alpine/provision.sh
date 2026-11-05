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
# kivora-reserved may use the blocks kept on the data disk: when the worker fills
# it, the desktop, the credentials, the audit log and the gateway still write.
# Fixed GID: it is recorded in the disk's filesystem.
RESERVED_GROUP=kivora-reserved RESERVED_GID=1202
# Fixed UID, like the accounts below: its state is on the data disk.
WEB_USER=${WEB_USER:-kivora-web}
WEB_UID=${WEB_UID:-1104}
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
# The proxy of the agent's shell commands. Its list stays on the data disk, and
# only root changes it.
PROXY_USER=${PROXY_USER:-kivora-proxy}
PROXY_DIR=/var/lib/kivora-proxy
WORKSPACE=${WORKSPACE:-/workspace}
DATA_DIR=/data
DATA_SERIAL=kivora-data
SERVICES="kivora-hidepid kivora-data kivora-firewall kivora-proxy kivora-display kivora-wm kivora-browser kivora-vnc kivora-web"

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

# blank_disk tells whether the disk's first MiB holds nothing but zeros, as on a
# disk never written. A read that fails does not count as blank.
blank_disk() {
	head_file=$(mktemp)
	if dd if="$1" of="$head_file" bs=1M count=1 2>/dev/null && [ "$(wc -c <"$head_file")" = 1048576 ] &&
		[ -z "$(tr -d '\0' <"$head_file" | head -c 1)" ]; then
		rm -f "$head_file"
		return 0
	fi
	rm -f "$head_file"
	return 1
}

# bind_to_data moves DIR onto the data disk (first time only) and bind-mounts
# it back in place, without setuid programs or device files. kivora-data mounts it
# at boot, not the system: see that service.
bind_to_data() {
	dir=$1 target=$DATA_DIR$1
	sed -i -E "s|^$target $dir none bind(,noatime,nosuid,nodev)? 0 0$|$target $dir none bind,noauto,noatime,nosuid,nodev 0 0|" /etc/fstab
	if ! mountpoint -q "$dir"; then
		if [ ! -d "$target" ]; then
			mkdir -p "$target"
			[ -d "$dir" ] && cp -a "$dir/." "$target/"
		fi
		mkdir -p "$dir"
		grep -q "^$target $dir " /etc/fstab || echo "$target $dir none bind,noauto,noatime,nosuid,nodev 0 0" >>/etc/fstab
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
	bash curl libgcc libstdc++ ripgrep xclip file poppler-utils \
	e2fsprogs acl nftables qemu-guest-agent logrotate

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

# The network of the VM carries no NTP. Its clock follows the host's, through KVM.
rc-update -a del chronyd >/dev/null 2>&1 || true
rc-service -q chronyd stop 2>/dev/null || true

# Empty /tmp at boot: Chromium leaves directories there when it crashes.
sed -i 's/^wipe_tmp=.*/wipe_tmp="YES"/' /etc/conf.d/bootmisc

# Boot without waiting at the boot menu.
[ -f /boot/limine/limine.conf ] && sed -i 's/^timeout: .*/timeout: 1/' /boot/limine/limine.conf

# Errors on a filesystem must not stop the boot before SSH and the network come up:
# the admin commands are what can repair them, or rebuild this system disk. The
# data disk is checked and mounted apart, by kivora-data.
sed -i 's/^#*fsck_abort_on_errors=.*/fsck_abort_on_errors="NO"/' /etc/conf.d/fsck
for svc in root localmount; do
	grep -q '^rc_need="!fsck"' "/etc/conf.d/$svc" 2>/dev/null || echo 'rc_need="!fsck"' >>"/etc/conf.d/$svc"
	grep -q '^rc_after="fsck"' "/etc/conf.d/$svc" || echo 'rc_after="fsck"' >>"/etc/conf.d/$svc"
done

# Backups freeze the data disk's filesystem through the guest agent.
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
getent group "$RESERVED_GROUP" >/dev/null || addgroup -g "$RESERVED_GID" "$RESERVED_GROUP"
for member in "$AGENT_USER:$WS_GROUP" "$WORKER_USER:$WS_GROUP" "$AGENT_USER:$CTL_GROUP" "$AGENT_USER:$RESERVED_GROUP"; do
	addgroup "${member%%:*}" "${member#*:}" 2>/dev/null || true
done

if ! id "$WEB_USER" >/dev/null 2>&1; then
	log "creating system user $WEB_USER"
	adduser -S -D -H -u "$WEB_UID" -h /var/lib/$WEB_USER -s /sbin/nologin "$WEB_USER"
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
if ! id "$PROXY_USER" >/dev/null 2>&1; then
	log "creating system user $PROXY_USER"
	adduser -S -D -H -G "$PROXY_USER" -h /nonexistent -s /sbin/nologin "$PROXY_USER" 2>/dev/null ||
		{ addgroup -S "$PROXY_USER"; adduser -S -D -H -G "$PROXY_USER" -h /nonexistent -s /sbin/nologin "$PROXY_USER"; }
fi
for member in "$VAULT_USER" "$AUDIT_USER" "$WEB_USER"; do
	addgroup "$member" "$RESERVED_GROUP" 2>/dev/null || true
done
mkdir -p "$VAULT_DIR" "$KEYS_DIR" "$AUDIT_DIR" "$PROXY_DIR"

disk=$(data_disk)
fresh_data=0
if [ -n "$disk" ]; then
	log "setting up data disk $disk"
	# A disk is only new if nothing was ever written on it. Without a filesystem
	# that blkid recognizes, it may simply be damaged, and repairable.
	if ! mountpoint -q "$DATA_DIR" && [ -z "$(blkid "$disk")" ]; then
		if ! blank_disk "$disk"; then
			log "error: the data disk $disk holds data, but no filesystem this system recognizes"
			log "not formatting it: your files may still be there. Run kivora repair-data, or bring a backup back with kivora restore --yes"
			exit 1
		fi
		mkfs.ext4 -q -L "$DATA_SERIAL" "$disk"
		# Set the reserved blocks and their group while the disk is unmounted:
		# the kernel reads s_resgid at mount time only. Run on a mounted disk (as
		# below) it reaches the on-disk superblock but not the live mount, so the
		# reserve stays root-only until the next boot, and a worker that fills
		# /workspace can starve the vault, the audit log, the gateway and the
		# browser in the meantime.
		tune2fs -m 5 -g "$RESERVED_GID" "$disk" >/dev/null
		fresh_data=1
	fi
	mkdir -p "$DATA_DIR"
	sed -i -E "s|^LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noatime(,nosuid,nodev)? 0 2$|LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noauto,noatime,nosuid,nodev 0 0|" /etc/fstab
	grep -q "^LABEL=$DATA_SERIAL " /etc/fstab ||
		echo "LABEL=$DATA_SERIAL $DATA_DIR ext4 defaults,noauto,noatime,nosuid,nodev 0 0" >>/etc/fstab
	mountpoint -q "$DATA_DIR" || mount "$DATA_DIR"
	mount -o remount,noatime,nosuid,nodev "$DATA_DIR"
	tune2fs -m 5 -g "$RESERVED_GID" "$disk" >/dev/null
	if ! mountpoint -q "/home/$AGENT_USER"; then
		for svc in $SERVICES; do rc-service -q "$svc" stop 2>/dev/null || true; done
	fi
	bind_to_data "/home/$AGENT_USER"
	bind_to_data "/home/$WORKER_USER"
	bind_to_data "$WORKSPACE"
	bind_to_data "$VAULT_DIR"
	bind_to_data "$KEYS_DIR"
	bind_to_data "$AUDIT_DIR"
	bind_to_data "$PROXY_DIR"
	# The input owner lives there: rebuilding the system must not forget it.
	if ! mountpoint -q "/var/lib/$WEB_USER"; then
		rc-service -q kivora-web stop 2>/dev/null || true
	fi
	bind_to_data "/var/lib/$WEB_USER"
else
	log "no data disk (serial $DATA_SERIAL): keeping data on the system disk"
fi

# A data disk from an older VM may carry other UIDs.
for entry in "$AGENT_USER:/home/$AGENT_USER" "$AGENT_USER:$WORKSPACE" "$WORKER_USER:/home/$WORKER_USER" "$VAULT_USER:$VAULT_DIR" "$AUDIT_USER:$AUDIT_DIR" "$WEB_USER:/var/lib/$WEB_USER"; do
	user=${entry%%:*} dir=${entry#*:}
	if [ -d "$dir" ] && [ "$(stat -c %u "$dir")" != "$(id -u "$user")" ]; then
		log "fixing ownership of $dir"
		chown -Rh "$user:$(id -gn "$user")" "$dir"
	fi
done
chmod 0700 "/home/$AGENT_USER" "/home/$WORKER_USER" "$VAULT_DIR" "$AUDIT_DIR" "/var/lib/$WEB_USER"

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

# Nothing is allowed until the human says so.
chown root:root "$PROXY_DIR"
chmod 0755 "$PROXY_DIR"
[ -e "$PROXY_DIR/allowlist" ] || : >"$PROXY_DIR/allowlist"
chown root:root "$PROXY_DIR/allowlist"
chmod 0644 "$PROXY_DIR/allowlist"

log "preparing $WORKSPACE"
mkdir -p "$WORKSPACE"
chown "$AGENT_USER:$WS_GROUP" "$WORKSPACE"
chmod 2770 "$WORKSPACE"
# Default ACLs give the shared group read/write on everything created inside,
# whatever the creating process's umask. On the directory itself only: root going
# through a tree the worker writes would follow what the worker swaps in meanwhile.
setfacl -P -m "g:$WS_GROUP:rwX" -m "d:u::rwX,d:g::rwX,d:g:$WS_GROUP:rwX,d:o::-" "$WORKSPACE"

log "installing services"
cp -R rootfs/. /
chmod 0755 /etc/init.d/kivora-* /usr/local/bin/kivora-*
chmod 0400 /etc/doas.d/*.conf
chmod 0755 /etc/chromium/policies /etc/chromium/policies/managed
chmod 0644 /etc/chromium/policies/managed/*.json
chmod 0644 /etc/sysctl.d/kivoradesk.conf
sysctl -q -p /etc/sysctl.d/kivoradesk.conf
doas -C /etc/doas.d/kivora-mcp.conf
# No menu on the desktop: its default entries can only stop the window manager.
sed -i '/<context name="Root">/,/<\/context>/{/<mousebind/,/<\/mousebind>/d}' /etc/xdg/openbox/rc.xml
# On a new data disk the agent starts with the desktop. Otherwise a missing state
# means something was lost, and the gateway starts paused.
if [ "$fresh_data" = 1 ]; then
	su -s /bin/sh "$WEB_USER" -c "/usr/local/bin/kivora-web init-control" ||
		log "warning: could not hand the desktop to the agent: it stays paused until you do it"
fi
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
# A start that failed earlier may have left its supervisor behind, which OpenRC
# no longer knows of: the next start would find the program running and fail.
for svc in $SERVICES; do
	pid=$(cat "/run/supervise-$svc.pid" 2>/dev/null) || continue
	kill "$pid" 2>/dev/null || true
	for _ in $(seq 50); do
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.1
	done
	kill -9 "$pid" 2>/dev/null || true
	rm -f "/run/supervise-$svc.pid"
done
for svc in dbus crond $SERVICES; do
	rc-update add "$svc" default >/dev/null
	rc-service -q "$svc" start
done

log "provisioning done"
