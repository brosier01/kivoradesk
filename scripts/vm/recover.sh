#!/usr/bin/env bash
# Rebuild the VM's system disk from the reference image ("recover"), keeping
# the data disk. Use it when the system is broken. Running work is
# interrupted, and the web gateway gets a new sign-in token.
#
# usage: recover.sh --yes
set -euo pipefail
source "$(dirname "$0")/common.sh"
here=$(dirname "$0")

[[ ${1:-} == --yes ]] || die "this rebuilds the VM's system (data is kept); re-run with --yes"
"$here/destroy.sh" --yes --keep-data
"$here/create.sh"
"$here/provision.sh"
"$here/doctor.sh"
log "recovered; get the new sign-in URL with scripts/vm/web-url.sh"
