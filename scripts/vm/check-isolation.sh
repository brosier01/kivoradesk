#!/usr/bin/env bash
# Check that the VM reaches the Internet but none of the private services the
# host can reach: the host itself, the LAN router and anything on the host's
# other networks. Each private target is first checked from the host, so a
# target that is down does not count as blocked. Targets in the allowlist
# (scripts/vm/allow.sh) are reported, not checked.
set -euo pipefail
source "$(dirname "$0")/common.sh"

reachable_from_host() { timeout 2 bash -c "</dev/tcp/$1/$2" 2>/dev/null; }
reachable_from_vm() { "$(dirname "$0")/ssh.sh" "nc -z -w2 $1 $2" 2>/dev/null; }

targets=()
gateway=$(virsh_ net-dumpxml "$LIBVIRT_NETWORK" | sed -n "s/.*<ip address='\([0-9.]*\)'.*/\1/p" | head -n1)
targets+=("$gateway:22")
for addr in $(ip -4 -o addr show scope global | awk '{split($4, a, "/"); print a[1]}'); do
	[[ $addr == "$gateway" ]] || targets+=("$addr:22")
done
router=$(ip -4 route show default | awk '{print $3; exit}')
[[ -n $router ]] && targets+=("$router:53" "$router:80" "$router:443")

failed=0
for target in "${targets[@]}"; do
	host=${target%:*} port=${target#*:}
	if [[ -f $KIVORA_ALLOWLIST ]] && grep -qE "^($host|$host:$port)( |$)" "$KIVORA_ALLOWLIST"; then
		printf '%-8s %s (in the allowlist)\n' allowed "$target"
	elif ! reachable_from_host "$host" "$port"; then
		printf '%-8s %s (not reachable from the host either)\n' skip "$target"
	elif reachable_from_vm "$host" "$port"; then
		printf '%-8s %s reachable from the VM\n' FAIL "$target"
		failed=1
	else
		printf '%-8s %s blocked\n' ok "$target"
	fi
done

if "$(dirname "$0")/ssh.sh" 'nslookup example.com >/dev/null && curl -fsS -o /dev/null -m 10 https://example.com'; then
	printf '%-8s %s\n' ok "Internet (DNS + HTTPS) reachable"
else
	printf '%-8s %s\n' FAIL "Internet not reachable from the VM"
	failed=1
fi

exit $failed
