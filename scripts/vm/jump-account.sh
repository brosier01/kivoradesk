#!/usr/bin/env bash
# Print the commands that create an account on this server for the laptop's
# jumps to the VM: no shell, and SSH forwarding to the VM only. Without it, the
# laptop jumps through an account that can administer the server, libvirt
# included. They need sudo, so this script only prints them: read them, run them,
# then use the account with ssh-config.sh.
#
# usage: jump-account.sh PUBLIC_KEY_FILE [ACCOUNT]
#   PUBLIC_KEY_FILE  the laptop's public key
#   ACCOUNT          name of the account (default: kivora-jump)
set -euo pipefail
source "$(dirname "$0")/common.sh"

[[ $# -ge 1 && -f $1 ]] || die "usage: jump-account.sh PUBLIC_KEY_FILE [ACCOUNT]"
key=$(head -n1 "$1")
[[ $key == ssh-* || $key == ecdsa-* || $key == sk-* ]] || die "$1 does not look like an SSH public key"
account=${2:-kivora-jump}
[[ $account =~ ^[a-z_][a-z0-9_-]*$ ]] || die "invalid account name $account"

cat <<EOF
sudo useradd --system --create-home --shell /usr/sbin/nologin $account
sudo install -d -m 755 -o root -g root /home/$account/.ssh
sudo tee /home/$account/.ssh/authorized_keys >/dev/null <<'KEYS'
restrict,port-forwarding,permitopen="$VM_IP:22" $key
KEYS
EOF
