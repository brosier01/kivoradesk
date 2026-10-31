#!/usr/bin/env bash
# Print the one-time sign-in URL of the web gateway. Open it in a browser on a
# machine that has the SSH tunnel to the VM (see scripts/vm/ssh-config.sh).
#
# usage: web-url.sh [--rotate]
#   --rotate  replace the token first, signing out every browser
set -euo pipefail
source "$(dirname "$0")/common.sh"

case ${1:-} in
"") ;;
--rotate)
	"$(dirname "$0")/ssh.sh" 'doas rm -f /var/lib/kivora-web/token && doas rc-service -q kivora-web restart' ||
		die "could not rotate the gateway token"
	log "token rotated: every browser must sign in again"
	;;
*) die "usage: web-url.sh [--rotate]" ;;
esac

# The gateway writes a new token shortly after it starts.
token=
for _ in $(seq 10); do
	token=$("$(dirname "$0")/ssh.sh" 'doas cat /var/lib/kivora-web/token 2>/dev/null') && break
	sleep 1
done
[[ -n $token ]] || die "could not read the gateway token (is the VM provisioned?)"
echo "http://localhost:$WEB_PORT/?token=$token"
