#!/usr/bin/env bash
# Allow an SSH public key to log in to the VM as the agent user, for example
# the laptop's key. The key is stored on the data disk and survives rebuilds.
set -euo pipefail
source "$(dirname "$0")/common.sh"

[[ $# -eq 1 && -f $1 ]] || die "usage: authorize-key.sh PUBLIC_KEY_FILE"
key=$(head -n1 "$1")
[[ $key == ssh-* || $key == ecdsa-* || $key == sk-* ]] || die "$1 does not look like an SSH public key"

printf '%s\n' "$key" | "$(dirname "$0")/ssh.sh" "doas sh -c '
	set -e
	dir=/home/$VM_AGENT/.ssh
	install -d -m 700 -o $VM_AGENT -g $VM_AGENT \$dir
	touch \$dir/authorized_keys
	read -r key
	grep -qxF \"\$key\" \$dir/authorized_keys || printf \"%s\n\" \"\$key\" >>\$dir/authorized_keys
	chown $VM_AGENT:$VM_AGENT \$dir/authorized_keys
	chmod 600 \$dir/authorized_keys
'"
log "key authorized for $VM_AGENT@$VM_NAME"
