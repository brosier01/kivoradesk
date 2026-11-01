#!/usr/bin/env bash
# Print a sign-in URL for the web gateway: it works once, for 5 minutes. Open it
# in a browser on a machine that has the SSH tunnel to the VM (see
# scripts/vm/ssh-config.sh). The browser then stays signed in for 30 days.
#
# usage: web-url.sh [--rotate]
#   --rotate  sign out every browser first
set -euo pipefail
source "$(dirname "$0")/common.sh"

gateway() { "$(dirname "$0")/ssh.sh" "doas -u kivora-web /usr/local/bin/kivora-web $1"; }

case ${1:-} in
"") ;;
--rotate)
	gateway sign-out || die "could not sign out the browsers"
	log "every browser must sign in again"
	;;
*) die "usage: web-url.sh [--rotate]" ;;
esac

code=$(gateway login-code) || die "could not get a sign-in code (is the VM provisioned?)"
echo "http://kivora.localhost:$WEB_PORT/login?code=$code"
