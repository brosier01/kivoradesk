#!/usr/bin/env bash
# Build the guest binaries, copy image/alpine to the VM and run the guest
# provisioning script.
set -euo pipefail
source "$(dirname "$0")/common.sh"

ip=$(vm_ip)
[[ -n $ip ]] || die "no IP address found for VM '$VM_NAME' (is it running?)"
mapfile -t ssh_opts < <(vm_ssh_opts)

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp -R "$REPO_ROOT/image/alpine/." "$stage/"
mkdir -p "$stage/rootfs/usr/local/bin"

log "building guest binaries"
version=$(git -C "$REPO_ROOT" describe --always --dirty 2>/dev/null || echo dev)
for cmd in kivora-web kivora-mcp kivora-vault kivora-audit kivora-proxy; do
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$stage/rootfs/usr/local/bin/$cmd" "./cmd/$cmd")
done

log "provisioning $VM_NAME ($ip)"
tar -C "$stage" -cf - . |
	ssh "${ssh_opts[@]}" "$VM_ADMIN@$ip" '
		set -e
		dir=$(mktemp -d)
		trap "doas rm -rf $dir" EXIT
		tar -C "$dir" -xf -
		cd "$dir" && doas sh ./provision.sh
	'
