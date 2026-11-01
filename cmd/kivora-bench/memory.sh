# Memory of the VM, run as root: "used" (MemTotal - MemAvailable) and the
# proportional set size of each component, in KiB.
awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{print "used", t-a}' /proc/meminfo
for d in /proc/[0-9]*; do
	pss=$(awk '/^Pss:/{print $2}' "$d/smaps_rollup" 2>/dev/null)
	[ -n "$pss" ] || continue
	case "$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null)" in
	*/chromium/* | chromium*) g=chromium ;;
	*Xvfb*) g=xvfb ;;
	*x11vnc*) g=x11vnc ;;
	*kivora-web*) g=kivora-web ;;
	*kivora-mcp*) g=kivora-mcp ;;
	*openbox*) g=openbox ;;
	*sshd*) g=sshd ;;
	*) g=other ;;
	esac
	echo "$g $pss"
done | awk '{s[$1]+=$2; t+=$2} END{for (g in s) print g, s[g]; print "total", t}'
