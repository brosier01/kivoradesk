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
for svc in kivora-hidepid kivora-firewall kivora-proxy kivora-display kivora-wm kivora-browser kivora-vnc kivora-web qemu-guest-agent sshd; do
	if rc-service "$svc" status >/dev/null 2>&1; then echo "ok service $svc"; else echo "fail service $svc is not running"; fi
done
for mnt in /data /home/agent /workspace /var/lib/kivora-vault /var/lib/kivora-audit /var/lib/kivora-proxy /etc/ssh/authorized_keys; do
	mountpoint -q "$mnt" && echo "ok mount $mnt" || echo "fail mount $mnt is not mounted"
done
grep -E ' /(workspace|data) ' /proc/mounts | grep -qv 'nosuid.*nodev' && echo "fail data disk mounted with setuid programs or devices" || echo "ok data disk without setuid programs or devices"
[ "$(sysctl -n fs.protected_regular 2>/dev/null)" = 2 ] && echo "ok kernel settings applied" || echo "fail kernel settings are not applied"
[ "$(stat -c '%U %a' /var/lib/kivora-audit 2>/dev/null)" = "kivora-audit 700" ] && echo "ok audit log out of the agent's reach" || echo "fail audit log is not private to its account"
[ "$(stat -c '%U %a' /var/lib/kivora-vault 2>/dev/null)" = "kivora-vault 700" ] && echo "ok credentials private to the vault account" || echo "fail credentials are not private to the vault account"
[ "$(nft list table inet kivoradesk 2>/dev/null | grep -c 'fib daddr type local')" = 3 ] && nft list table inet kivoradesk | grep -q "meta skuid $(id -u worker) reject" && echo "ok firewall rules loaded" || echo "fail firewall rules are missing"
[ "$(stat -c '%U %a' /var/lib/kivora-proxy/allowlist 2>/dev/null)" = "root 644" ] && echo "ok shell network: $(grep -c . /var/lib/kivora-proxy/allowlist) site(s) allowed" || echo "fail list of the shell's sites is missing or not owned by root"
grep -q ' /proc proc .*hidepid' /proc/mounts && echo "ok processes hidden between accounts" || echo "fail processes are not hidden between accounts"
[ "$(stat -c '%U' /etc/ssh/authorized_keys/agent 2>/dev/null)" = root ] && echo "ok SSH keys out of the agent's reach" || echo "fail SSH keys of the agent are not owned by root"
su -s /bin/sh worker -c '/usr/local/bin/kivora-mcp worker exec -- true' 2>/dev/null | grep -q '"exit_code":0' && echo "ok shell commands run isolated" || echo "fail shell commands cannot run: unprivileged namespaces unavailable?"
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
log=/var/lib/kivora-audit/audit-$today.jsonl
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
