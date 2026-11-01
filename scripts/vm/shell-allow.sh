#!/usr/bin/env bash
# Choose the sites that the agent's shell commands may reach, over HTTPS. They
# reach nothing else, and nothing at first: downloads go through browser_download,
# and the browser is not concerned. Changes apply at once.
#
# Allow as little as possible: a site that accepts uploads, such as a code host,
# also lets a command misled by a web page send the workspace there.
#
# usage: shell-allow.sh add HOST[:PORT]      e.g. pypi.org or *.pythonhosted.org
#        shell-allow.sh remove HOST[:PORT]
#        shell-allow.sh list
set -euo pipefail
source "$(dirname "$0")/common.sh"

usage="usage: shell-allow.sh add HOST[:PORT] | remove HOST[:PORT] | list"
valid() {
	[[ $1 =~ ^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$ ]] || return 1
	local port=${1##*:}
	[[ $1 != *:* ]] || ((port >= 1 && port <= 65535))
}

case ${1:-} in
list)
	"$(dirname "$0")/ssh.sh" 'cat /var/lib/kivora-proxy/allowlist'
	;;
add | remove)
	[[ $# -eq 2 ]] && valid "$2" || die "$usage"
	"$(dirname "$0")/ssh.sh" "doas sh -s -- $1 $(printf '%q' "$2")" <<'SCRIPT'
list=/var/lib/kivora-proxy/allowlist
case $1 in
add) grep -qxF -- "$2" "$list" || printf '%s\n' "$2" >>"$list" ;;
remove)
	grep -qxF -- "$2" "$list" || { echo "error: $2 is not in the list" >&2; exit 1; }
	grep -vxF -- "$2" "$list" >"$list.new" || true
	chmod 644 "$list.new"
	mv "$list.new" "$list"
	;;
esac
SCRIPT
	log "$1: $2"
	;;
*) die "$usage" ;;
esac
