#!/usr/bin/env bash
# Print the SSH configuration to add to ~/.ssh/config on the laptop. The VM is
# only reachable from this host, so the laptop jumps through it.
#
# usage: ssh-config.sh HOST_ADDRESS [HOST_USER]
#   HOST_ADDRESS  how the laptop reaches this host (LAN IP, VPN IP or name)
#   HOST_USER     account on this host (default: current user); prefer one
#                 limited to this jump, see jump-account.sh
set -euo pipefail
source "$(dirname "$0")/common.sh"

[[ $# -ge 1 ]] || die "usage: ssh-config.sh HOST_ADDRESS [HOST_USER]"
host_addr=$1
host_user=${2:-$(id -un)}

cat <<EOF
# KivoraDesk: the homelab host, used as a jump host.
Host $VM_NAME-host
    HostName $host_addr
    User $host_user

# KivoraDesk VM. "ssh -N $VM_NAME" opens the live view tunnel on
# http://localhost:$WEB_PORT (sign in with the URL from scripts/vm/web-url.sh).
Host $VM_NAME
    HostName $VM_IP
    User $VM_AGENT
    ProxyJump $VM_NAME-host
    LocalForward $WEB_PORT 127.0.0.1:$WEB_PORT
    ServerAliveInterval 30
EOF
