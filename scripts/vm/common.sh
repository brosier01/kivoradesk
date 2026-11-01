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

# Fixed MAC and DHCP reservation, so the VM keeps the same address and the
# laptop's SSH configuration stays valid across rebuilds.
VM_MAC=${VM_MAC:-52:54:00:6b:64:01}
VM_IP=${VM_IP:-192.168.122.50}

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
KIVORA_CACHE_DIR=$KIVORA_DATA_DIR/cache
KIVORA_SSH_KEY=$KIVORA_DATA_DIR/ssh/id_ed25519

# Encrypted backups of the data disk. Without the passphrase file, backups
# cannot be restored: keep a copy of it somewhere else.
KIVORA_BACKUP_DIR=${KIVORA_BACKUP_DIR:-$KIVORA_DATA_DIR/backups}
KIVORA_BACKUP_KEEP=${KIVORA_BACKUP_KEEP:-7}
KIVORA_BACKUP_PASSPHRASE=${KIVORA_BACKUP_PASSPHRASE:-$KIVORA_DATA_DIR/backup.passphrase}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

virsh_() { virsh --connect "$LIBVIRT_URI" "$@"; }

dhcp_host_xml() { printf "<host mac='%s' name='%s' ip='%s'/>" "$VM_MAC" "$VM_NAME" "$VM_IP"; }

# vm_ip prints the first IPv4 address leased to the VM, or nothing.
vm_ip() {
	virsh_ domifaddr "$VM_NAME" --source lease 2>/dev/null |
		awk '/ipv4/ { split($4, a, "/"); print a[1]; exit }'
}

vm_ssh_opts() {
	printf '%s\n' -i "$KIVORA_SSH_KEY" \
		-o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null \
		-o LogLevel=ERROR \
		-o ConnectTimeout=5
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
	xml=$(virsh_ net-dumpxml "$LIBVIRT_NETWORK")
	gateway=$(sed -n "s/.*<ip address='\([0-9.]*\)'.*/\1/p" <<<"$xml" | head -n1)
	[[ -n $gateway ]] || die "could not read the gateway address of network $LIBVIRT_NETWORK"
	if [[ -f $KIVORA_ALLOWLIST ]]; then
		while read -r entry _; do
			[[ -z $entry || $entry == \#* ]] && continue
			rules+=$(allow_rule "$entry")$'\n'
		done <"$KIVORA_ALLOWLIST"
	fi
	xml=$(sed "s|@GATEWAY@|$gateway|g" "$REPO_ROOT/host/libvirt/nwfilter-no-lan.xml")
	# Redefining an existing filter must keep its UUID.
	uuid=$(virsh_ nwfilter-dumpxml "$NWFILTER" 2>/dev/null | sed -n 's|.*<uuid>\(.*\)</uuid>.*|\1|p')
	if [[ -n $uuid ]]; then
		xml=${xml/"<filter name='$NWFILTER' chain='root'>"/"<filter name='$NWFILTER' chain='root'>"$'\n'"  <uuid>$uuid</uuid>"}
	fi
	xml=${xml/"  <!-- @ALLOWLIST@ -->"/"$rules"}
	virsh_ nwfilter-define /dev/stdin <<<"$xml" >/dev/null
}
