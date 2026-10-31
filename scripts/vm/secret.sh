#!/usr/bin/env bash
# Manage the credentials the agent may fill into web pages without seeing
# them. Each credential is bound to the sites (origins) where it may be used.
#
# usage: secret.sh set NAME ORIGIN [ORIGIN...]   prompts for the value
#        secret.sh list
#        secret.sh delete NAME
#
# example: secret.sh set github-password https://github.com
set -euo pipefail
source "$(dirname "$0")/common.sh"

run() { "$(dirname "$0")/ssh.sh" "doas -u $VM_AGENT /usr/local/bin/kivora-mcp secrets $(printf '%q ' "$@")"; }

case ${1:-} in
set)
	[[ $# -ge 3 ]] || die "usage: secret.sh set NAME ORIGIN [ORIGIN...]"
	# read fails at end of input without a final newline, value read or not.
	read -rsp "Value for $2 (not shown): " value || true
	[[ -t 0 ]] && echo >&2
	[[ -n $value ]] || die "empty value"
	printf '%s' "$value" | run "$@"
	;;
list | delete) run "$@" ;;
*) die "usage: secret.sh set NAME ORIGIN... | list | delete NAME" ;;
esac
