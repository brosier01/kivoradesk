# Shared settings for the host-side VM scripts. Sourced, not executed.

ALPINE_BRANCH=${ALPINE_BRANCH:-v3.24}
ALPINE_RELEASE=${ALPINE_RELEASE:-3.24.3}
ALPINE_IMAGE=${ALPINE_IMAGE:-alpine-${ALPINE_RELEASE}-x86_64-cloudinit-r0.qcow2}
ALPINE_MIRROR=${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}

VM_NAME=${VM_NAME:-kivoradesk}
VM_VCPUS=${VM_VCPUS:-2}
VM_MEMORY_MB=${VM_MEMORY_MB:-4096}
VM_DISK_GB=${VM_DISK_GB:-16}
VM_DATA_GB=${VM_DATA_GB:-16}
VM_ADMIN=${VM_ADMIN:-kivora}
VM_AGENT=${VM_AGENT:-agent}

# Fixed MAC and DHCP reservation (VM_IP, set below), so the VM keeps the same
# address and the laptop's SSH configuration stays valid across rebuilds.
VM_MAC=${VM_MAC:-52:54:00:6b:64:01}

# Port of the web gateway inside the VM.
WEB_PORT=${WEB_PORT:-6080}

LIBVIRT_URI=${LIBVIRT_URI:-qemu:///system}
LIBVIRT_POOL=${LIBVIRT_POOL:-kivoradesk}
LIBVIRT_POOL_DIR=${LIBVIRT_POOL_DIR:-/var/lib/libvirt/images/kivoradesk}
LIBVIRT_NETWORK=${LIBVIRT_NETWORK:-default}

# libvirt network filter keeping the VM off private networks
# (host/libvirt/nwfilter-no-lan.xml), and the private services the human
# allowed anyway (scripts/vm/allow.sh).
NWFILTER=${NWFILTER:-kivoradesk-no-lan}
KIVORA_ALLOWLIST=${KIVORA_ALLOWLIST:-${XDG_DATA_HOME:-$HOME/.local/share}/kivoradesk/allowlist}

KIVORA_DATA_DIR=${KIVORA_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/kivoradesk}
# Private to this account: it holds the SSH key, the passphrase of the backups
# and the backups themselves.
mkdir -p "$KIVORA_DATA_DIR"
chmod 700 "$KIVORA_DATA_DIR"
KIVORA_CACHE_DIR=$KIVORA_DATA_DIR/cache
KIVORA_SSH_KEY=$KIVORA_DATA_DIR/ssh/id_ed25519
KIVORA_KNOWN_HOSTS=$KIVORA_DATA_DIR/ssh/known_hosts

# Encrypted backups of the data disk. Without the passphrase file, backups
# cannot be restored: keep a copy of it somewhere else.
KIVORA_BACKUP_DIR=${KIVORA_BACKUP_DIR:-$KIVORA_DATA_DIR/backups}
KIVORA_BACKUP_KEEP=${KIVORA_BACKUP_KEEP:-7}
KIVORA_BACKUP_PASSPHRASE=${KIVORA_BACKUP_PASSPHRASE:-$KIVORA_DATA_DIR/backup.passphrase}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

virsh_() { virsh --connect "$LIBVIRT_URI" "$@"; }

# network_gateway prints the IPv4 address of the libvirt network, or nothing.
network_gateway() {
	virsh_ net-dumpxml "$LIBVIRT_NETWORK" 2>/dev/null | awk -F"'" '/<ip address=/ && !gateway { gateway = $2 } END { if (gateway) print gateway }'
}

# default_vm_ip prints address .50 on the libvirt network, whatever its range.
default_vm_ip() {
	local gateway
	gateway=$(network_gateway || true)
	gateway=${gateway:-192.168.122.1}
	printf '%s.50' "${gateway%.*}"
}
VM_IP=${VM_IP:-$(default_vm_ip)}

dhcp_host_xml() { printf "<host mac='%s' name='%s' ip='%s'/>" "$VM_MAC" "$VM_NAME" "$VM_IP"; }

# vm_ip prints the first IPv4 address leased to the VM, or nothing. awk reads to
# the end: with pipefail, leaving early can kill virsh and the whole script.
vm_ip() {
	virsh_ domifaddr "$VM_NAME" --source lease 2>/dev/null |
		awk '/ipv4/ && !ip { split($4, a, "/"); ip = a[1] } END { if (ip) print ip }'
}

# The VM's host key is kept once seen: these scripts run root commands in the
# VM and send it credentials.
vm_ssh_opts() {
	printf '%s\n' -i "$KIVORA_SSH_KEY" \
		-o StrictHostKeyChecking=accept-new \
		-o "UserKnownHostsFile=$KIVORA_KNOWN_HOSTS" \
		-o LogLevel=ERROR \
		-o ConnectTimeout=5
}

# forget_host_key is for a new system disk, which comes with a new host key.
forget_host_key() {
	ssh-keygen -q -R "$VM_IP" -f "$KIVORA_KNOWN_HOSTS" >/dev/null 2>&1 || true
	rm -f "$KIVORA_KNOWN_HOSTS.old"
}

# allow_rule turns an allowlist entry (IP[/BITS][:PORT]) into an nwfilter rule.
allow_rule() {
	local entry=$1 addr port="" ip bits=32
	addr=${entry%%:*}
	[[ $entry == *:* ]] && port=${entry##*:}
	ip=${addr%%/*}
	[[ $addr == */* ]] && bits=${addr##*/}
	if [[ -n $port ]]; then
		printf "  <rule action='accept' direction='out' priority='200'>\n    <tcp dstipaddr='%s' dstipmask='%s' dstportstart='%s' dstportend='%s' comment='allowlist: %s'/>\n  </rule>\n" \
			"$ip" "$bits" "$port" "$port" "$entry"
	else
		printf "  <rule action='accept' direction='out' priority='200'>\n    <all dstipaddr='%s' dstipmask='%s' comment='allowlist: %s'/>\n  </rule>\n" \
			"$ip" "$bits" "$entry"
	fi
}

# define_nwfilter (re)defines the network filter from the template and the
# allowlist. libvirt applies a redefinition to the running VM at once.
define_nwfilter() {
	local xml gateway uuid rules="" entry
	gateway=$(network_gateway)
	[[ -n $gateway ]] || die "could not read the gateway address of network $LIBVIRT_NETWORK"
	if [[ -f $KIVORA_ALLOWLIST ]]; then
		while read -r entry _; do
			[[ -z $entry || $entry == \#* ]] && continue
			rules+=$(allow_rule "$entry")$'\n'
		done <"$KIVORA_ALLOWLIST"
	fi
	xml=$(sed -e "s|@NWFILTER@|$NWFILTER|g" -e "s|@GATEWAY@|$gateway|g" -e "s|@VM_IP@|$VM_IP|g" "$REPO_ROOT/host/libvirt/nwfilter-no-lan.xml")
	# Redefining an existing filter must keep its UUID.
	uuid=$({ virsh_ nwfilter-dumpxml "$NWFILTER" 2>/dev/null || true; } | sed -n 's|.*<uuid>\(.*\)</uuid>.*|\1|p')
	if [[ -n $uuid ]]; then
		xml=${xml/"<filter name='$NWFILTER' chain='root'>"/"<filter name='$NWFILTER' chain='root'>"$'\n'"  <uuid>$uuid</uuid>"}
	fi
	xml=${xml/"  <!-- @ALLOWLIST@ -->"/"$rules"}
	virsh_ nwfilter-define /dev/stdin <<<"$xml" >/dev/null
}
