#!/usr/bin/env bash
# Print the one-time sign-in URL of the web gateway. Open it in a browser on a
# machine that has the SSH tunnel to the VM (see scripts/vm/ssh-config.sh).
set -euo pipefail
source "$(dirname "$0")/common.sh"

token=$("$(dirname "$0")/ssh.sh" 'doas cat /var/lib/kivora-web/token') ||
	die "could not read the gateway token (is the VM provisioned?)"
echo "http://localhost:$WEB_PORT/?token=$token"
