#!/usr/bin/env bash
# Let the VM reach specific private services despite the network isolation,
# for example a test server on the LAN. Entries apply at once to the running
# VM and are kept in the allowlist file on the host.
#
# usage: allow.sh add IP[/BITS][:PORT] [COMMENT...]   e.g. 192.168.1.20:8080 staging
#        allow.sh remove IP[/BITS][:PORT]
#        allow.sh list
# Without a port, every TCP and UDP port of the address is allowed.
set -euo pipefail
source "$(dirname "$0")/common.sh"

valid() {
	[[ $1 =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}(/([0-9]|[12][0-9]|3[0-2]))?(:[0-9]{1,5})?$ ]] || return 1
	local port=${1##*:}
	[[ $1 != *:* ]] || ((port >= 1 && port <= 65535))
}

case ${1:-} in
list)
	[[ -s $KIVORA_ALLOWLIST ]] && cat "$KIVORA_ALLOWLIST" || log "allowlist is empty: the VM reaches no private service"
	;;
add)
	entry=${2:-}
	valid "$entry" || die "usage: allow.sh add IP[/BITS][:PORT] [COMMENT...]"
	mkdir -p "$(dirname "$KIVORA_ALLOWLIST")"
	touch "$KIVORA_ALLOWLIST"
	if grep -q "^$entry\b" "$KIVORA_ALLOWLIST"; then
		log "$entry is already allowed"
	else
		echo "$entry ${*:3}" | sed 's/ *$//' >>"$KIVORA_ALLOWLIST"
	fi
	define_nwfilter
	log "the VM may now reach $entry"
	;;
remove)
	entry=${2:-}
	valid "$entry" || die "usage: allow.sh remove IP[/BITS][:PORT]"
	[[ -f $KIVORA_ALLOWLIST ]] && grep -q "^$entry\b" "$KIVORA_ALLOWLIST" || die "$entry is not in the allowlist"
	grep -v "^$entry\b" "$KIVORA_ALLOWLIST" >"$KIVORA_ALLOWLIST.new" || true
	mv "$KIVORA_ALLOWLIST.new" "$KIVORA_ALLOWLIST"
	define_nwfilter
	log "$entry is blocked again"
	;;
*) die "usage: allow.sh add ENTRY [COMMENT] | remove ENTRY | list" ;;
esac
