#!/bin/sh
# Checks of "kivora doctor" inside the VM, as root. Each line starts with ok, warn or fail.
for svc in kivora-hidepid kivora-data kivora-firewall kivora-proxy kivora-display kivora-wm kivora-browser kivora-vnc kivora-web crond qemu-guest-agent sshd; do
	if rc-service "$svc" status >/dev/null 2>&1; then echo "ok service $svc"; else echo "fail service $svc is not running"; fi
done
for mnt in /data /home/agent /home/worker /workspace /var/lib/kivora-vault /var/lib/kivora-audit /var/lib/kivora-proxy /var/lib/kivora-web /etc/ssh/authorized_keys; do
	options=$(awk -v m="$mnt" '$2 == m { o = $4 } END { print o }' /proc/mounts)
	case ,$options, in
	,,) echo "fail mount $mnt is not mounted" ;;
	*,ro,*) echo "fail mount $mnt is read-only" ;;
	*,nosuid,*nodev,*) echo "ok mount $mnt" ;;
	*) echo "fail mount $mnt allows setuid programs or devices" ;;
	esac
done
settings=ok
for setting in fs.protected_regular=2 fs.protected_fifos=2 fs.protected_hardlinks=1 fs.protected_symlinks=1 \
	kernel.kptr_restrict=2 dev.tty.ldisc_autoload=0 kernel.shm_rmid_forced=1 kernel.io_uring_disabled=2; do
	if [ "$(sysctl -n "${setting%%=*}" 2>/dev/null)" != "${setting#*=}" ]; then
		echo "fail kernel setting ${setting%%=*} is not ${setting#*=}"
		settings=fail
	fi
done
[ $settings = ok ] && echo "ok kernel settings applied"
[ "$(stat -c '%U %a' /var/lib/kivora-audit 2>/dev/null)" = "kivora-audit 700" ] && echo "ok audit log out of the agent's reach" || echo "fail audit log is not private to its account"
[ "$(stat -c '%U %a' /var/lib/kivora-vault 2>/dev/null)" = "kivora-vault 700" ] && echo "ok credentials private to the vault account" || echo "fail credentials are not private to the vault account"
[ "$(nft list table inet kivoradesk 2>/dev/null | grep -c 'fib daddr type local')" = 3 ] && nft list table inet kivoradesk | grep -q "meta skuid $(id -u worker) reject" && echo "ok firewall rules loaded" || echo "fail firewall rules are missing"
[ "$(stat -c '%U %a' /var/lib/kivora-proxy/allowlist 2>/dev/null)" = "root 644" ] && echo "ok shell network: $(grep -c . /var/lib/kivora-proxy/allowlist) site(s) allowed" || echo "fail list of the shell's sites is missing or not owned by root"
su -s /bin/sh worker -c 'nc -z -w2 127.0.0.1 3128' </dev/null 2>/dev/null && echo "ok shell network proxy answers" || echo "fail shell network proxy does not answer"
grep -q ' /proc proc .*hidepid' /proc/mounts && echo "ok processes hidden between accounts" || echo "fail processes are not hidden between accounts"
[ "$(stat -c '%U' /etc/ssh/authorized_keys/agent 2>/dev/null)" = root ] && echo "ok SSH keys out of the agent's reach" || echo "fail SSH keys of the agent are not owned by root"
sshd_agent=$(sshd -T -C user=agent,host=kivoradesk,addr=10.86.0.1 2>/dev/null)
sshd=ok
for line in "forcecommand /usr/local/bin/kivora-session" "permittty no" "passwordauthentication no" "permitrootlogin no" "permitopen 127.0.0.1:680 localhost:680"; do
	printf '%s\n' "$sshd_agent" | grep -qxF "$line" || {
		echo "fail SSH server: no \"$line\" for the agent account"
		sshd=fail
	}
done
[ $sshd = ok ] && echo "ok laptops only get the MCP server, file transfers and the tunnel"
# In the namespaces of a command, a new user namespace must be refused.
su -s /bin/sh worker -c "/usr/local/bin/kivora-mcp worker exec -- 'unshare -Ur true 2>/dev/null && exit 9; exit 0'" </dev/null 2>/dev/null | grep -q '"exit_code":0' && echo "ok shell commands run isolated" || echo "fail shell commands do not run isolated: unprivileged namespaces unavailable?"
su -s /bin/sh worker -c '/usr/local/bin/kivora-mcp worker valid-json /etc/chromium/policies/managed/kivora.json' </dev/null >/dev/null 2>&1 && echo "ok browser policies installed" || echo "fail browser policies are missing or unreadable: Chromium would ignore them"
# The kernel reads the reserve's group when it mounts the disk.
grep -q ' /data ext4 .*resgid=1202' /proc/mounts && echo "ok disk reserve kept for the audit log and the credentials" || echo "fail disk reserve is root's only: kivora ssh 'doas reboot'"
df -P /workspace / | awk 'NR > 1 { pct = $5 + 0; msg = sprintf("disk %s %d%% used, %d MiB free", $6, pct, $4 / 1024);
	print (pct >= 90 ? "warn " : "ok ") msg }'
free -m | awk '/^Mem:/ { print "ok memory " $3 " MiB used, " $7 " MiB available" }'
# The browser reads hostile pages: nothing else tells the human it lags behind.
if timeout 20 apk update -q >/dev/null 2>&1; then
	late=$(apk version -l '<' 2>/dev/null | awk 'NR > 1 { n++ } END { print n + 0 }')
	if [ "$late" -gt 0 ]; then
		echo "warn $late package(s) can be upgraded: kivora ssh 'doas apk upgrade --available'"
	else
		echo "ok packages up to date"
	fi
else
	echo "warn could not check for package upgrades"
fi
[ -d "/lib/modules/$(uname -r)" ] || echo "warn a new kernel is installed: kivora ssh 'doas reboot'"
# Chromium may still be starting right after a deployment.
browser=fail
for _ in $(seq 20); do
	wget -qO- http://127.0.0.1:9222/json/version 2>/dev/null | grep -q Browser && browser=ok && break
	sleep 1
done
[ $browser = ok ] && echo "ok browser DevTools answer" || echo "fail browser DevTools do not answer after 20 s"
code=$(wget -S -qO- http://127.0.0.1:680/ 2>&1 | awk '/HTTP\// { print $2; exit }')
[ "$code" = 401 ] && echo "ok web gateway answers (sign-in required)" || echo "fail web gateway answers ${code:-nothing}"
[ -S /run/kivora/control.sock ] && echo "ok control socket present" || echo "fail control socket missing"
v=$(/usr/local/bin/kivora-mcp -version 2>/dev/null) && echo "ok MCP server $v" || echo "fail MCP server does not run"
pgrep -x xclip >/dev/null && echo "warn a stale xclip owns the clipboard (it blocks the live view input)"
today=$(date -u +%F)
log=/var/lib/kivora-audit/audit-$today.jsonl
if [ -f "$log" ]; then
	echo "ok audit today: $(grep -c . "$log") calls, $(grep -c '"ok":false' "$log") errors"
fi
