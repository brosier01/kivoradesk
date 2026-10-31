#!/bin/sh
# Provision an Alpine guest as a KivoraDesk desktop. Runs as root, idempotent.
# Expects to be run from the directory that contains rootfs/.
set -eu

AGENT_USER=${AGENT_USER:-agent}
WORKSPACE=${WORKSPACE:-/workspace}

log() { printf '==> %s\n' "$*" >&2; }

log "installing packages"
apk update -q
apk add -q \
	xvfb openbox xdotool x11vnc scrot xwininfo xprop \
	chromium font-dejavu dbus \
	bash curl libgcc libstdc++ ripgrep

if ! id "$AGENT_USER" >/dev/null 2>&1; then
	log "creating unprivileged user $AGENT_USER"
	adduser -D -s /bin/bash -g "KivoraDesk agent" "$AGENT_USER"
fi

log "preparing $WORKSPACE"
mkdir -p "$WORKSPACE"
chown "$AGENT_USER:$AGENT_USER" "$WORKSPACE"
chmod 0750 "$WORKSPACE"

log "installing services"
cp -R rootfs/. /
chmod 0755 /etc/init.d/kivora-*

for svc in dbus kivora-display kivora-wm kivora-browser kivora-vnc; do
	rc-update add "$svc" default >/dev/null
	rc-service "$svc" restart >/dev/null
done

log "provisioning done"
