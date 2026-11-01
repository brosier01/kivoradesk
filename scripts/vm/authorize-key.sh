#!/usr/bin/env bash
# Allow an SSH public key to log in to the VM as the agent user, for example
# the laptop's key. The key is stored on the data disk and survives rebuilds,
# in a file the agent account can read but not change.
set -euo pipefail
source "$(dirname "$0")/common.sh"

[[ $# -eq 1 && -f $1 ]] || die "usage: authorize-key.sh PUBLIC_KEY_FILE"
key=$(head -n1 "$1")
[[ $key == ssh-* || $key == ecdsa-* || $key == sk-* ]] || die "$1 does not look like an SSH public key"

printf '%s\n' "$key" | "$(dirname "$0")/ssh.sh" "doas sh -c '
	set -e
	keys=/etc/ssh/authorized_keys/$VM_AGENT
	touch \$keys
	read -r key
	grep -qxF \"\$key\" \$keys || printf \"%s\n\" \"\$key\" >>\$keys
	chown root:$VM_AGENT \$keys
	chmod 640 \$keys
'"
log "key authorized for $VM_AGENT@$VM_NAME"
