#!/usr/bin/env bash
# Diagnose the KivoraDesk VM from the host: libvirt, network filter, SSH,
# services, guest agent, disk, memory, browser, gateway and MCP server.
# Exits non-zero if a check fails.
set -uo pipefail
source "$(dirname "$0")/common.sh"

failed=0
ok() { printf 'ok    %s\n' "$*"; }
warn() { printf 'WARN  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failed=1; }

state=$(virsh_ domstate "$VM_NAME" 2>/dev/null)
if [[ $state != running ]]; then
	fail "VM $VM_NAME is ${state:-not defined} (start it: virsh -c $LIBVIRT_URI start $VM_NAME)"
	exit 1
fi
ok "VM $VM_NAME is running"

xml=$(virsh_ dumpxml "$VM_NAME")
if [[ $xml == *"filterref filter='$NWFILTER'"* ]]; then
	ok "network filter $NWFILTER applied"
else
	fail "network filter $NWFILTER missing: the VM can reach private networks"
fi

ip=$(vm_ip)
[[ $ip == "$VM_IP" ]] && ok "address $ip" || warn "address ${ip:-none}, expected $VM_IP"

if virsh_ qemu-agent-command "$VM_NAME" '{"execute":"guest-ping"}' >/dev/null 2>&1; then
	ok "guest agent answers (clean shutdown and snapshots available)"
else
	warn "guest agent does not answer"
fi

if ! report=$("$(dirname "$0")/ssh.sh" 'doas sh -s' 2>/dev/null <<'EOF'
for svc in kivora-hidepid kivora-firewall kivora-display kivora-wm kivora-browser kivora-vnc kivora-web qemu-guest-agent sshd; do
	if rc-service "$svc" status >/dev/null 2>&1; then echo "ok service $svc"; else echo "fail service $svc is not running"; fi
done
for mnt in /data /home/agent /workspace /var/lib/kivora-vault; do
	mountpoint -q "$mnt" && echo "ok mount $mnt" || echo "fail mount $mnt is not mounted"
done
[ "$(stat -c '%U %a' /var/lib/kivora-vault 2>/dev/null)" = "kivora-vault 700" ] && echo "ok credentials private to the vault account" || echo "fail credentials are not private to the vault account"
[ "$(nft list table inet kivoradesk 2>/dev/null | grep -c 'fib daddr type local')" = 3 ] && echo "ok firewall rules loaded" || echo "fail firewall rules are missing"
grep -q ' /proc proc .*hidepid' /proc/mounts && echo "ok processes hidden between accounts" || echo "fail processes are not hidden between accounts"
[ -f /etc/chromium/policies/managed/kivora.json ] && echo "ok browser policies installed" || echo "fail browser policies are missing"
df -P /workspace / | awk 'NR > 1 { pct = $5 + 0; msg = sprintf("disk %s %d%% used, %d MiB free", $6, pct, $4 / 1024);
	print (pct >= 90 ? "warn " : "ok ") msg }'
free -m | awk '/^Mem:/ { print "ok memory " $3 " MiB used, " $7 " MiB available" }'
# Chromium may still be starting right after a deployment.
browser=fail
for i in $(seq 20); do
	wget -qO- http://127.0.0.1:9222/json/version 2>/dev/null | grep -q Browser && browser=ok && break
	sleep 1
done
[ $browser = ok ] && echo "ok browser DevTools answer" || echo "fail browser DevTools do not answer after 20 s"
code=$(wget -S -qO- http://127.0.0.1:6080/ 2>&1 | awk '/HTTP\// { print $2; exit }')
[ "$code" = 401 ] && echo "ok web gateway answers (sign-in required)" || echo "fail web gateway answers ${code:-nothing}"
[ -S /run/kivora/control.sock ] && echo "ok control socket present" || echo "fail control socket missing"
v=$(/usr/local/bin/kivora-mcp -version 2>/dev/null) && echo "ok MCP server $v" || echo "fail MCP server does not run"
pgrep -x xclip >/dev/null && echo "warn a stale xclip owns the clipboard (it blocks the live view input)"
today=$(date -u +%F)
log=/home/agent/.local/state/kivora/audit/audit-$today.jsonl
if [ -f "$log" ]; then
	echo "ok audit today: $(grep -c . "$log") calls, $(grep -c '"ok":false' "$log") errors"
fi
EOF
); then
	fail "cannot run the checks over SSH"
	exit 1
fi
while read -r level msg; do
	case $level in
	ok) ok "$msg" ;;
	warn) warn "$msg" ;;
	*) fail "$msg" ;;
	esac
done <<<"$report"

exit $failed
