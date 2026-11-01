#!/usr/bin/env bash
# Manage the credentials the agent may fill into web pages without seeing
# them. Each credential is bound to the sites (origins) where it may be used,
# and goes into password fields only. --field text also allows login, email and
# one-time code fields, whose content a site may display: keep it for values
# that are not really secret, such as a login name.
#
# usage: secret.sh set [--field text] NAME ORIGIN [ORIGIN...]   prompts for the value
#        secret.sh list
#        secret.sh delete NAME
#
# example: secret.sh set github-password https://github.com
#          secret.sh set --field text github-login https://github.com
set -euo pipefail
source "$(dirname "$0")/common.sh"

run() { "$(dirname "$0")/ssh.sh" "doas -u kivora-vault /usr/local/bin/kivora-vault $(printf '%q ' "$@")"; }

case ${1:-} in
set)
	shift
	field=()
	if [[ ${1:-} == --field ]]; then
		field=(-field "${2:-}")
		shift $(($# < 2 ? $# : 2))
	fi
	[[ $# -ge 2 ]] || die "usage: secret.sh set [--field text] NAME ORIGIN [ORIGIN...]"
	# read fails at end of input without a final newline, value read or not.
	read -rsp "Value for $1 (not shown): " value || true
	[[ -t 0 ]] && echo >&2
	[[ -n $value ]] || die "empty value"
	printf '%s' "$value" | run set "${field[@]}" "$@"
	;;
list | delete) run "$@" ;;
*) die "usage: secret.sh set [--field text] NAME ORIGIN... | list | delete NAME" ;;
esac
