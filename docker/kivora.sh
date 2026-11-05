#!/bin/bash
# Administer the VM from inside the container:
#
#   docker compose exec kivoradesk kivora COMMAND
#
# Run it without a command for the list.
STATE=/var/lib/kivoradesk
ADMIN_DIR=$STATE/admin
VM_DIR=$STATE/vm
# What these commands hand to the VM's side: they alone write here. They never write
# in the VM's directory, nor follow a path in it: should the VM get out of QEMU, it
# could leave a link there to their keys or their backups.
SHARED_DIR=$STATE/shared
HOST_ADDRESSES=/var/lib/kivoradesk-host/addresses
KEY=$ADMIN_DIR/id_ed25519
HOST_KEY=$ADMIN_DIR/vm_host_ed25519
KNOWN_HOSTS=$ADMIN_DIR/known_hosts
PASSPHRASE=$ADMIN_DIR/backup.passphrase
BACKUPS=$ADMIN_DIR/backups
PROVISIONED=$ADMIN_DIR/provisioned
ALLOWLIST=$SHARED_DIR/allowlist
READY=/tmp/kivora-ready
DEGRADED=/tmp/kivora-degraded
ADMIN=kivora
AGENT=agent
SSH_PORT=2222
# The live view: on the laptop's side of the tunnel, and in the VM.
WEB_PORT=6080
GUEST_WEB_PORT=680
INSTALL_TIMEOUT=1500
GUEST_IP=10.86.0.2
GATEWAY_IP=10.86.0.1

log() { printf '==> %s\n' "$*" >&2; }

[[ ${1:-} == --source ]] && return 0

set -euo pipefail

if [[ $(id -u) == 0 ]]; then
	exec su-exec kivora "$0" "$@"
fi

# Everything here is private. The few files QEMU must open say so themselves.
umask 077

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# The VM's host key is created in the volume before its first boot, so it is known
# from the start and survives a rebuild of the system. These commands run root
# commands in the VM: another key is refused.
SSH_OPTIONS=(-p "$SSH_PORT" -i "$KEY" -F /dev/null -o UserKnownHostsFile="$KNOWN_HOSTS" -o StrictHostKeyChecking=yes
	-o LogLevel=ERROR -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
admin_ssh() { ssh "${SSH_OPTIONS[@]}" -o BatchMode=yes -o ConnectTimeout=5 "$ADMIN@127.0.0.1" "$@"; }

vmctl() { kivora-vmctl -qmp "$VM_DIR/qmp.sock" -qga "$VM_DIR/qga.sock" "$@"; }

# Everything cleanup removes, whatever happens: a copy of a disk is in clear.
cleanup_paths=()
cleanup() { ((${#cleanup_paths[@]} == 0)) || rm -rf -- "${cleanup_paths[@]}"; }
trap cleanup EXIT

lock() {
	exec 9>"$ADMIN_DIR/lock"
	flock -n 9 || die "a backup or a restore is already running"
}

# Create the keys and the seed of a new system disk.
seed() {
	[[ -f $KEY ]] || ssh-keygen -q -t ed25519 -N '' -C kivoradesk-admin -f "$KEY"
	[[ -f $HOST_KEY ]] || ssh-keygen -q -t ed25519 -N '' -C kivoradesk -f "$HOST_KEY"
	printf '[127.0.0.1]:%s %s\n' "$SSH_PORT" "$(cut -d' ' -f1,2 "$HOST_KEY.pub")" >"$KNOWN_HOSTS"

	local work
	work=$(mktemp -d)
	cleanup_paths+=("$work")
	# The network has no DHCP: the address is fixed, and cloud-init does not write
	# resolv.conf for a fixed address.
	sed -e "s|@HOSTNAME@|kivoradesk|g" \
		-e "s|@ADMIN@|$ADMIN|g" \
		-e "s|@SSH_PUBKEY@|$(cat "$KEY.pub")|g" \
		-e 's|^write_files:$|write_files:\n  - path: /etc/resolv.conf\n    content: "nameserver '"$GATEWAY_IP"'\\n"|' \
		/opt/kivoradesk/user-data.tmpl >"$work/user-data"
	{
		echo 'ssh_keys:'
		echo '  ed25519_private: |'
		sed 's/^/    /' "$HOST_KEY"
		echo "  ed25519_public: $(cut -d' ' -f1,2 "$HOST_KEY.pub")"
	} >>"$work/user-data"
	printf 'instance-id: kivoradesk-%s\nlocal-hostname: kivoradesk\n' "$(date +%s)" >"$work/meta-data"
	cat >"$work/network-config" <<-EOF
		version: 2
		ethernets:
		  eth0:
		    addresses: [$GUEST_IP/24]
		    gateway4: $GATEWAY_IP
	EOF
	# Readable by QEMU, the only reader, which holds this key anyway through the disk
	# it boots.
	xorriso -as mkisofs -quiet -output "$SHARED_DIR/seed.part" -volid cidata -joliet -rock \
		"$work/user-data" "$work/meta-data" "$work/network-config" 2>/dev/null
	chmod 640 "$SHARED_DIR/seed.part"
	mv "$SHARED_DIR/seed.part" "$SHARED_DIR/seed.iso"
	rm -f "$PROVISIONED"
}

wait_for_ssh() {
	for _ in $(seq 100); do
		admin_ssh true >/dev/null 2>&1 && return 0
		sleep 3
	done
	die "the VM did not become reachable over SSH"
}

# Alpine removes the running kernel's modules when it installs a new kernel: what
# needs one loaded, such as the firewall, fails until the VM has restarted on it.
restart_on_new_kernel() {
	if ! admin_ssh '[ -d "/lib/modules/$(uname -r)" ]'; then
		log "new kernel installed: restarting the VM"
		admin_ssh 'doas reboot' >/dev/null 2>&1 || true
		sleep 5
		wait_for_ssh
	fi
}

# Install in the VM what this image brings, if that changed.
install_guest() {
	local want first_boot=0
	[[ -f $PROVISIONED ]] || first_boot=1
	log "waiting for SSH"
	wait_for_ssh
	((!first_boot)) || admin_ssh 'cloud-init status --wait; true' >/dev/null 2>&1 || true

	want=$(cat /opt/kivoradesk/guest.sum)
	[[ $want != "$(cat "$PROVISIONED" 2>/dev/null || true)" ]] || return 0
	# A new VM is as old as its image, and its browser reads the web from the start.
	log "upgrading the Alpine packages"
	admin_ssh 'doas apk update -q && doas apk upgrade -q --available'
	restart_on_new_kernel
	log "installing KivoraDesk in the VM"
	tar -C /opt/kivoradesk/guest -cf - . | admin_ssh '
		set -e
		dir=$(mktemp -d)
		trap "doas rm -rf $dir" EXIT
		tar -C "$dir" -xf -
		cd "$dir" && doas sh ./provision.sh
	'
	printf '%s\n' "$want" >"$PROVISIONED"
	restart_on_new_kernel
}

authorize_key() {
	printf '%s\n' "$1" | admin_ssh "doas sh -c '
		set -e
		keys=/etc/ssh/authorized_keys/$AGENT
		touch \$keys
		read -r key
		grep -qxF \"\$key\" \$keys || printf \"%s\n\" \"\$key\" >>\$keys
		chown root:$AGENT \$keys
		chmod 640 \$keys
	'"
}

add_laptop() {
	local usage='usage: kivora add-laptop "ssh-ed25519 AAAA... laptop" [SERVER_ADDRESS]'
	local key=${1:-} address=${2:-} bind=${KIVORA_BIND:-127.0.0.1}
	[[ $# -le 2 && $key != *$'\n'* ]] || die "$usage"
	[[ $key == ssh-* || $key == ecdsa-* || $key == sk-* ]] || die "$usage"
	printf '%s\n' "$key" | ssh-keygen -l -f - >/dev/null 2>&1 || die "this is not a valid public key"
	bind=${bind//[\[\]]/}
	if [[ -z $address ]]; then
		# Where the port is published, unless that means every address.
		address=$bind
		if [[ $bind == 0.0.0.0 || $bind == :: ]]; then
			address=127.0.0.1
			log "warning: the port is published on every address: give the one the laptop reaches, as a second argument"
		fi
	fi
	[[ $address =~ ^[A-Za-z0-9.:-]+$ ]] || die "invalid address: $address"
	authorize_key "$key"
	if [[ $bind == 127.* && $address != 127.* && $address != localhost ]]; then
		log "warning: the port is only published on this server: set KIVORA_BIND in .env (see the README), or add a ProxyJump"
	fi
	log "key authorized. Now run this on the laptop:"
	cat <<-EOF

		touch ~/.ssh/config && chmod go-w ~/.ssh/config
		cat >>~/.ssh/config <<'CONFIG'

		# KivoraDesk. "ssh -N kivoradesk" opens the live view on http://kivora.localhost:$WEB_PORT
		Host kivoradesk
		    HostName $address
		    Port ${KIVORA_SSH_PORT:-$SSH_PORT}
		    User $AGENT
		    HostKeyAlias kivoradesk
		    LocalForward $WEB_PORT 127.0.0.1:$GUEST_WEB_PORT
		    ServerAliveInterval 30
		CONFIG
		echo 'kivoradesk $(cut -d' ' -f1,2 "$HOST_KEY.pub")' >>~/.ssh/known_hosts
	EOF
}

web_url() {
	local usage="usage: kivora web-url [--rotate] [--port LAPTOP_PORT]" port=$WEB_PORT rotate=0 code
	while (($#)); do
		case $1 in
		--rotate)
			rotate=1
			shift
			;;
		--port)
			[[ ${2:-} =~ ^[1-9][0-9]{0,4}$ ]] && ((${2:-} <= 65535)) || die "$usage"
			port=$2
			shift 2
			;;
		*) die "$usage" ;;
		esac
	done
	if ((rotate)); then
		admin_ssh 'doas -u kivora-web /usr/local/bin/kivora-web sign-out' || die "could not sign out the browsers"
		log "every browser must sign in again"
	fi
	code=$(admin_ssh 'doas -u kivora-web /usr/local/bin/kivora-web login-code') || die "could not get a sign-in code"
	echo "http://kivora.localhost:$port/login?code=$code"
}

# The keys of the laptops, by the fingerprint ssh-keygen -l gives. A key removed
# also cuts every open connection, or a stolen laptop would keep its own.
laptops() {
	local usage="usage: kivora laptops list | remove SHA256:FINGERPRINT"
	local keys=/etc/ssh/authorized_keys/$AGENT line target found=0
	local -a kept=()
	case ${1:-} in
	list)
		[[ $# -eq 1 ]] || die "$usage"
		while IFS= read -r line; do
			[[ -z $line ]] || printf '%s\n' "$line" | ssh-keygen -l -f - || true
		done < <(admin_ssh "doas cat $keys")
		;;
	remove)
		[[ $# -eq 2 ]] || die "$usage"
		target=$2
		while IFS= read -r line; do
			[[ -n $line ]] || continue
			if [[ $(printf '%s\n' "$line" | ssh-keygen -l -f - 2>/dev/null | cut -d' ' -f2) == "$target" ]]; then
				found=1
			else
				kept+=("$line")
			fi
		done < <(admin_ssh "doas cat $keys")
		((found)) || die "$target is not among the laptop keys: kivora laptops list"
		if ((${#kept[@]})); then printf '%s\n' "${kept[@]}"; fi | admin_ssh "doas sh -c '
			set -e
			cat >$keys.new
			chown root:$AGENT $keys.new
			chmod 640 $keys.new
			mv $keys.new $keys
			pkill -u $AGENT -x sshd-session || true
		'"
		log "removed $target; open connections of the laptops were cut"
		;;
	*) die "$usage" ;;
	esac
}

# Credentials the agent may fill into web pages without seeing them. Each is bound
# to the sites where it may be used, and goes into password fields only. --field
# text also allows login, email and one-time code fields, whose content a site may
# display: keep it for values that are not really secret, such as a login name.
secret() {
	local usage="usage: kivora secret set [--field text] NAME ORIGIN [ORIGIN...] | list | delete NAME"
	vault() { admin_ssh "doas -u kivora-vault /usr/local/bin/kivora-vault $(printf '%q ' "$@")"; }
	case ${1:-} in
	set)
		shift
		local field=() value
		if [[ ${1:-} == --field ]]; then
			field=(-field "${2:-}")
			shift $(($# < 2 ? $# : 2))
		fi
		[[ $# -ge 2 ]] || die "$usage"
		# read fails at end of input without a final newline, value read or not.
		IFS= read -rsp "Value for $1 (not shown): " value || true
		[[ -t 0 ]] && echo >&2
		[[ -n $value ]] || die "empty value"
		printf '%s' "$value" | vault set "${field[@]}" "$@"
		;;
	list | delete) vault "$@" ;;
	*) die "$usage" ;;
	esac
}

# The sites the agent's shell commands may reach, over HTTPS. They reach nothing
# else, and nothing at first. Allow as little as possible: a site that accepts
# uploads, such as a code host, also lets a command misled by a web page send the
# workspace there.
shell_allow() {
	local usage="usage: kivora shell-allow add HOST[:PORT] | remove HOST[:PORT] | list"
	case ${1:-} in
	list)
		[[ $# -eq 1 ]] || die "$usage"
		admin_ssh 'cat /var/lib/kivora-proxy/allowlist'
		;;
	add | remove)
		[[ $# -eq 2 && $2 =~ ^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$ ]] || die "$usage"
		if [[ $2 == *:* ]]; then
			local port=${2##*:}
			((10#$port >= 1 && 10#$port <= 65535)) || die "$usage"
		fi
		admin_ssh "doas sh -s -- $1 $(printf '%q' "$2")" <<-'SCRIPT'
			list=/var/lib/kivora-proxy/allowlist
			case $1 in
			add) grep -qxF -- "$2" "$list" || printf '%s\n' "$2" >>"$list" ;;
			remove)
				grep -qxF -- "$2" "$list" || { echo "error: $2 is not in the list" >&2; exit 1; }
				grep -vxF -- "$2" "$list" >"$list.new" || true
				chmod 644 "$list.new"
				mv "$list.new" "$list"
				;;
			esac
		SCRIPT
		log "$1: $2"
		;;
	*) die "$usage" ;;
	esac
}

# As strict as what kivora-net reads: an entry it ignored would only be told to the
# human through the container's logs.
valid_allow_entry() {
	local n='(0|[1-9][0-9]{0,2})'
	[[ $1 =~ ^$n\.$n\.$n\.$n(/([0-9]|[12][0-9]|3[0-2]))?(:([1-9][0-9]{0,4}))?$ ]] || return 1
	local i
	for i in 1 2 3 4; do
		((BASH_REMATCH[i] <= 255)) || return 1
	done
	[[ -z ${BASH_REMATCH[8]} ]] || ((BASH_REMATCH[8] <= 65535))
}

ip_number() {
	local IFS=.
	# shellcheck disable=SC2086
	set -- $1
	echo $((($1 << 24) | ($2 << 16) | ($3 << 8) | $4))
}

# An entry goes before the addresses of this server: a network can open it unnoticed.
warn_about_network() {
	local entry=${1%%:*} bits addr kind mask network
	[[ $entry == */* ]] || return 0
	bits=${entry#*/}
	if ((bits == 0)); then
		log "warning: this opens every address to the VM: private networks, this server, a cloud's metadata"
		return 0
	fi
	[[ -f $HOST_ADDRESSES ]] || return 0
	mask=$((0xffffffff << (32 - bits) & 0xffffffff))
	network=$(($(ip_number "${entry%/*}") & mask))
	while read -r addr kind; do
		if [[ $kind == address ]] && ((($(ip_number "$addr") & mask) == network)); then
			log "warning: this network holds $addr, an address of this server: the VM reaches its services"
		fi
	done <"$HOST_ADDRESSES"
}

# The private services the VM may reach despite its isolation, over TCP. Without a
# port, every port of the address. Changes apply to the next connection.
allow() {
	local usage="usage: kivora allow add IP[/BITS][:PORT] [COMMENT...] | remove IP[/BITS][:PORT] | list"
	local entry=${2:-}
	case ${1:-} in
	list)
		[[ -s $ALLOWLIST ]] && cat "$ALLOWLIST" || log "the VM reaches no private service"
		;;
	add)
		valid_allow_entry "$entry" || die "$usage"
		if [[ -f $ALLOWLIST ]] && awk -v e="$entry" '$1 == e { found = 1 } END { exit !found }' "$ALLOWLIST"; then
			log "$entry is already allowed"
			return 0
		fi
		{
			[[ ! -f $ALLOWLIST ]] || cat "$ALLOWLIST"
			echo "${*:2}"
		} >"$ALLOWLIST.new"
		chmod 640 "$ALLOWLIST.new"
		mv "$ALLOWLIST.new" "$ALLOWLIST"
		log "the VM may now reach $entry"
		warn_about_network "$entry"
		;;
	remove)
		valid_allow_entry "$entry" || die "$usage"
		[[ -f $ALLOWLIST ]] && awk -v e="$entry" '$1 == e { found = 1 } END { exit !found }' "$ALLOWLIST" ||
			die "$entry is not in the list"
		awk -v e="$entry" '$1 != e' "$ALLOWLIST" >"$ALLOWLIST.new"
		chmod 640 "$ALLOWLIST.new"
		mv "$ALLOWLIST.new" "$ALLOWLIST"
		log "$entry is blocked again"
		;;
	*) die "$usage" ;;
	esac
}

doctor() {
	local failed=0 report level msg
	ok() { printf 'ok    %s\n' "$*"; }
	warn() { printf 'WARN  %s\n' "$*"; }
	fail() {
		printf 'FAIL  %s\n' "$*"
		failed=1
	}
	local state
	ok "container $(kivora-net -version)"
	[[ ! -f $DEGRADED ]] || fail "$(cat "$DEGRADED")"
	state=$(vmctl status 2>/dev/null || true)
	if [[ $state == io-error ]]; then
		fail "VM is stopped on a disk error: the volume is probably full. Free some space, then: kivora resume"
		return 1
	fi
	if [[ $state != running ]]; then
		fail "VM is ${state:-not answering (or a backup is running: QEMU takes one client)}"
		return 1
	fi
	ok "VM is running"
	if [[ -f $HOST_ADDRESSES ]]; then
		ok "the VM's network knows $(grep -c ' address$' "$HOST_ADDRESSES" || true) address(es) of this server"
	else
		fail "addresses of this server unknown: the VM only reaches its allowed services (run docker compose up)"
	fi
	if vmctl agent 2>/dev/null; then
		ok "guest agent answers (consistent backups available)"
	else
		warn "guest agent does not answer"
	fi
	if ! report=$(admin_ssh 'doas sh -s' 2>/dev/null </opt/kivoradesk/doctor-guest.sh); then
		fail "cannot run the checks over SSH"
		return 1
	fi
	while read -r level msg; do
		case $level in
		ok) ok "$msg" ;;
		warn) warn "$msg" ;;
		*) fail "$msg" ;;
		esac
	done <<<"$report"
	return "$failed"
}

# Check that the VM reaches the Internet but no private service this container can
# reach: each target is first checked from here, so that one which is down does
# not count as blocked. The list of allowed services is reported, not checked.
check_isolation() {
	local failed=0 targets=() always=() target host port gateway="" addr kind
	local -A checked=()
	reachable_from_container() { { timeout 2 bash -c "</dev/tcp/$1/$2"; } 2>/dev/null; }
	reachable_from_vm() { admin_ssh "nc -z -w2 $1 $2" 2>/dev/null; }

	gateway=$(awk '$2 == "00000000" { g = $3 } END { if (g) print g }' /proc/net/route)
	if [[ -n $gateway ]]; then
		printf -v gateway '%d.%d.%d.%d' "0x${gateway:6:2}" "0x${gateway:4:2}" "0x${gateway:2:2}" "0x${gateway:0:2}"
		targets+=("$gateway:22")
	fi
	if [[ -f $HOST_ADDRESSES ]]; then
		while read -r addr kind; do
			if [[ $kind == address ]]; then
				targets+=("$addr:22")
			elif [[ $kind == router ]]; then
				targets+=("$addr:53" "$addr:80" "$addr:443")
			fi
		done <"$HOST_ADDRESSES"
	fi
	for target in "${targets[@]}"; do
		[[ -z ${checked[$target]:-} ]] || continue
		checked[$target]=1
		host=${target%:*} port=${target#*:}
		if [[ -f $ALLOWLIST ]] && awk -v a="$host" -v b="$target" '$1 == a || $1 == b { found = 1 } END { exit !found }' "$ALLOWLIST"; then
			printf '%-8s %s (in the allowlist)\n' allowed "$target"
		elif ! reachable_from_container "$host" "$port"; then
			printf '%-8s %s (not reachable from the container either)\n' skip "$target"
		elif reachable_from_vm "$host" "$port"; then
			printf '%-8s %s reachable from the VM\n' FAIL "$target"
			failed=1
		else
			printf '%-8s %s blocked\n' ok "$target"
		fi
	done
	# Not reachable from here on every server, and refused all the same.
	always=("$GATEWAY_IP:22" "$GATEWAY_IP:2222" 169.254.169.254:80)
	for target in "${always[@]}"; do
		if reachable_from_vm "${target%:*}" "${target#*:}"; then
			printf '%-8s %s reachable from the VM\n' FAIL "$target"
			failed=1
		else
			printf '%-8s %s blocked\n' ok "$target"
		fi
	done
	if admin_ssh 'nslookup example.com >/dev/null && curl -fsS -o /dev/null -m 10 https://example.com' >/dev/null 2>&1; then
		printf '%-8s %s\n' ok "Internet (DNS + HTTPS) reachable"
	else
		printf '%-8s %s\n' FAIL "Internet not reachable from the VM"
		failed=1
	fi
	return "$failed"
}

gpg_home() {
	GNUPGHOME=$(mktemp -d)
	export GNUPGHOME
	cleanup_paths+=("$GNUPGHOME")
}

# Most recent first. By name, which has the time of the backup: several can be
# written within a second, and an imported one has the time of its import.
list_backups() { ls -1 "$BACKUPS"/kivoradesk-data-*.qcow2.gpg 2>/dev/null | sort -r; }

# Two backups must not get the same name, within a second.
new_backup_path() {
	local out
	while :; do
		out=$BACKUPS/kivoradesk-data-$(date -u +%Y%m%dT%H%M%SZ).qcow2.gpg
		[[ -e $out ]] || break
		sleep 1
	done
	printf '%s\n' "$out"
}

# The caller holds the lock. Backups of the moment before a restore or a repair do
# not push older ones out: those may be the ones still good.
backup_now() {
	local rotate=${1:-rotate} keep=${KIVORA_BACKUP_KEEP:-7}
	[[ $rotate != rotate || $keep =~ ^[1-9][0-9]*$ ]] || die "KIVORA_BACKUP_KEEP must be a number from 1 up, not: $keep"
	local out copy size used free needed
	read -r size used < <(vmctl size data) || true
	[[ -n ${size:-} ]] || die "could not read the size of the data disk: does the VM run?"
	# The copy, then its encrypted version, at most what the disk takes now. A volume
	# that fills up stops the VM.
	free=$(($(df -Pk "$ADMIN_DIR" | awk 'NR == 2 { print $4 }') * 1024))
	needed=$((2 * used + (512 << 20)))
	((free >= needed)) || die "not enough space on the volume for a backup: $((needed >> 20)) MiB needed, $((free >> 20)) MiB free"
	if [[ ! -f $PASSPHRASE ]]; then
		head -c 32 /dev/urandom | base64 >"$PASSPHRASE"
		log "created the passphrase of the backups: copy it elsewhere, they cannot be restored without it"
	fi
	mkdir -p "$BACKUPS"
	out=$(new_backup_path)
	copy=$SHARED_DIR/backup.qcow2
	cleanup_paths+=("$copy" "$out.part")
	gpg_home

	rm -f "$copy"
	qemu-img create -q -f qcow2 "$copy" "$size"
	chmod 660 "$copy" # QEMU writes it
	log "copying the data disk"
	vmctl backup data /data "$copy"
	log "encrypting into $out"
	# 9>&-: the agent gpg leaves running would keep the lock.
	gpg --batch --yes --quiet --pinentry-mode loopback --passphrase-file "$PASSPHRASE" \
		--symmetric --cipher-algo AES256 --compress-level 1 -o "$out.part" "$copy" 9>&-
	mv "$out.part" "$out"
	rm -f "$copy"

	if [[ $rotate == rotate ]]; then
		local -a old
		mapfile -t old < <(list_backups | grep -vxF "$out" | tail -n +"$keep")
		((${#old[@]} == 0)) || {
			rm -f -- "${old[@]}"
			log "removed ${#old[@]} old backup(s)"
		}
	fi
	log "backup done: $out ($(du -h "$out" | cut -f1))"
}

# Over SSH, or by what needs no network. An error says nothing: SSH often ends with
# the VM.
shut_down() {
	admin_ssh 'doas poweroff' >/dev/null 2>&1 || vmctl shutdown >/dev/null 2>&1 || true
}

requests_pending() {
	[[ -e $SHARED_DIR/data.restore.qcow2 || -e $SHARED_DIR/recover || -e $SHARED_DIR/restart ]]
}

# Stop the VM after a request to the entrypoint, and wait for it to come back. The
# entrypoint removes the request before it starts the VM again.
restart_vm() {
	shut_down
	for _ in $(seq 60); do
		requests_pending || break
		sleep 1
	done
	if requests_pending; then
		log "the VM does not stop: forcing it"
		vmctl quit || true
	fi
	for _ in $(seq $((INSTALL_TIMEOUT + 300))); do
		[[ ! -f $READY ]] || return 0
		if ! requests_pending && [[ -f $DEGRADED ]]; then
			die "the VM did not come back: $(cat "$DEGRADED")"
		fi
		sleep 1
	done
	die "the VM did not come back: see docker compose logs"
}

restore() {
	if [[ ${1:-} == --list ]]; then
		list_backups || log "no backup"
		return 0
	fi
	local yes=0 backup_first=1 file=""
	while (($#)); do
		case $1 in
		--yes) yes=1 ;;
		--no-backup) backup_first=0 ;;
		-*) die "usage: kivora restore --list | --yes [--no-backup] [FILE]" ;;
		*)
			[[ -z $file ]] || die "usage: kivora restore --list | --yes [--no-backup] [FILE]"
			file=$1
			;;
		esac
		shift
	done
	((yes)) || die "this replaces /home/agent and /workspace in the VM; re-run with --yes [--no-backup] [BACKUP_FILE]"
	lock
	if [[ -z $file ]]; then
		file=$(list_backups | head -n1 || true)
	elif [[ $file != /* ]]; then
		file=$BACKUPS/$file
	fi
	[[ -f $file ]] || die "no backup to restore"
	[[ -f $PASSPHRASE ]] || die "no passphrase: bring it back with kivora import-passphrase"

	# The decrypted disk holds the credentials and the browser's logins.
	local work info
	work=$(mktemp -d "$ADMIN_DIR/restore.XXXXXX")
	cleanup_paths+=("$work")
	gpg_home
	log "decrypting $file"
	gpg --batch --quiet --pinentry-mode loopback --passphrase-file "$PASSPHRASE" -d -o "$work/data.qcow2" "$file" 9>&-
	qemu-img check -q -f qcow2 "$work/data.qcow2" || die "the backup is not a valid disk image"
	# A disk naming another file would let the VM read a file of this container.
	info=$(qemu-img info -f qcow2 --output=json "$work/data.qcow2")
	[[ $info != *backing-filename* && $info != *data-file* ]] || die "the backup refers to another file: refusing it"

	if ((backup_first)); then
		log "backing up the current data first"
		"$0" _backup no-rotation 9>&- ||
			die "the current data could not be backed up: free some space, or re-run with --no-backup to replace them without a copy"
	fi
	# QEMU reads it. This disk may be older than the system: the installation runs
	# again, to bring it up to date.
	chgrp kivora-vm "$work/data.qcow2"
	chmod 660 "$work/data.qcow2"
	rm -f "$PROVISIONED"
	mv "$work/data.qcow2" "$SHARED_DIR/data.restore.qcow2"
	log "restarting the VM on the restored data"
	restart_vm
	if [[ -e $SHARED_DIR/restore.failed ]]; then
		rm -f "$SHARED_DIR/restore.failed"
		die "the restored disk could not be put in place, and the VM runs on its previous data: is the volume full?"
	fi
	log "restored $file"
	log "the laptop keys are now those of the backup: kivora laptops list"
	doctor >/dev/null || log "warning: kivora doctor reports problems"
}

# A VM that stopped on a full volume stays stopped once there is space again.
resume() {
	[[ $# -eq 0 ]] || usage
	vmctl cont || die "the VM does not resume"
	log "the VM runs again: $(vmctl status)"
}

# From inside the VM, whose SSH works without this disk. It must be unmounted, so
# the services that use it stop, until the restart.
repair_data() {
	local yes=0 backup_first=1
	while (($#)); do
		case $1 in
		--yes) yes=1 ;;
		--no-backup) backup_first=0 ;;
		*) die "usage: kivora repair-data --yes [--no-backup]" ;;
		esac
		shift
	done
	((yes)) || die "this stops the VM's services and repairs its data disk, then restarts it; re-run with --yes [--no-backup]"
	lock
	if ((backup_first)); then
		log "copying the disk as it is first"
		"$0" _backup no-rotation 9>&- ||
			die "the disk could not be copied first: free some space, or re-run with --no-backup to repair it without a copy"
	fi
	log "repairing the data disk"
	admin_ssh 'doas sh -s' <<-'SCRIPT' || die "the disk could not be repaired: bring back a backup with kivora restore --yes --no-backup"
		disk=
		for serial in /sys/block/vd*/serial; do
			if [ "$(cat "$serial")" = kivora-data ]; then
				disk=/dev/$(basename "$(dirname "$serial")")
			fi
		done
		[ -n "$disk" ] || { echo "the data disk is not in the VM" >&2; exit 1; }
		for service in kivora-web kivora-vnc kivora-browser kivora-wm kivora-display kivora-proxy; do
			rc-service -q "$service" stop 2>/dev/null
		done
		pkill -KILL -u agent 2>/dev/null
		pkill -KILL -u worker 2>/dev/null
		sleep 1
		for dir in $(awk '$1 ~ "^/data/" { print $2 }' /etc/fstab) /data; do
			if mountpoint -q "$dir"; then
				umount "$dir" || { echo "$dir is in use" >&2; exit 1; }
			fi
		done
		e2fsck -fy "$disk"
		code=$?
		# Below 4, nothing is left to repair. Otherwise, it may be the superblock: its
		# copies are at these places, by block size.
		for copy in "32768 4096" "16384 2048" "8193 1024"; do
			[ "$code" -ge 4 ] || break
			set -- $copy
			echo "trying the copy of the superblock at block $1"
			e2fsck -fy -b "$1" -B "$2" "$disk"
			code=$?
		done
		[ "$code" -lt 4 ]
	SCRIPT
	rm -f "$PROVISIONED"
	: >"$SHARED_DIR/restart"
	log "restarting the VM on the repaired disk"
	restart_vm
	doctor >/dev/null || log "warning: kivora doctor reports problems"
	log "repaired"
}

recover() {
	[[ ${1:-} == --yes ]] || die "this rebuilds the VM's system (data is kept); re-run with --yes"
	lock
	: >"$SHARED_DIR/recover"
	log "rebuilding the VM's system"
	restart_vm
	doctor
	log "recovered"
}

import_backup() {
	lock
	mkdir -p "$BACKUPS"
	local out
	out=$(new_backup_path)
	cleanup_paths+=("$out.part")
	cat >"$out.part"
	[[ -s $out.part ]] || die "nothing received on standard input"
	mv "$out.part" "$out"
	log "imported as $(basename "$out"), the most recent backup"
}

import_passphrase() {
	[[ ! -f $PASSPHRASE || ${1:-} == --replace ]] ||
		die "a passphrase exists, and the backups here need it; re-run with --replace to discard it"
	cleanup_paths+=("$PASSPHRASE.new")
	cat >"$PASSPHRASE.new"
	[[ -s $PASSPHRASE.new ]] || die "nothing received on standard input"
	mv "$PASSPHRASE.new" "$PASSPHRASE"
	log "passphrase imported"
}

usage() {
	cat >&2 <<-EOF
		usage: kivora COMMAND, on the server, through: docker compose exec kivoradesk

		  add-laptop "PUBLIC_KEY" [SERVER_ADDRESS]    authorize a laptop, print its SSH setup
		  laptops list | remove SHA256:FINGERPRINT    the keys of the laptops
		  web-url [--rotate] [--port LAPTOP_PORT]     sign-in link of the live view
		  secret set [--field text] NAME ORIGIN...    store a credential (asks for its value)
		  secret list | delete NAME
		  shell-allow add|remove HOST[:PORT] | list   sites the agent's shell may reach
		  allow add|remove IP[/BITS][:PORT] | list    private services the VM may reach
		  backup                                      encrypted backup of the data disk
		  restore --list | --yes [--no-backup] [FILE] reset the data to a backup
		  import-backup < FILE                        bring a backup from elsewhere
		  import-passphrase [--replace] < FILE        bring its passphrase
		  recover --yes                               rebuild a broken system, keep the data
		  repair-data --yes [--no-backup]             repair a damaged data disk
		  resume                                      restart a VM stopped by a full volume
		  doctor                                      diagnose the VM and its services
		  check-isolation                             the VM reaches the Internet, not your network
		  ssh [COMMAND]                               admin shell in the VM
	EOF
	exit 1
}

command=${1:-}
[[ $# -eq 0 ]] || shift
case $command in
ssh)
	if [[ -t 0 ]]; then
		ssh -t "${SSH_OPTIONS[@]}" "$ADMIN@127.0.0.1" "$@"
	else
		admin_ssh "$@"
	fi
	;;
health)
	# Healthy once installed, so that "docker compose up --wait" means ready, and as
	# long as what the agent needs works.
	[[ -f $READY ]] && admin_ssh 'mountpoint -q /data && doas sh -c "rc-service kivora-firewall status && rc-service kivora-browser status && rc-service kivora-web status" >/dev/null'
	;;
add-laptop) add_laptop "$@" ;;
laptops) laptops "$@" ;;
web-url) web_url "$@" ;;
secret) secret "$@" ;;
shell-allow) shell_allow "$@" ;;
allow) allow "$@" ;;
backup)
	[[ $# -eq 0 ]] || usage
	lock
	backup_now
	;;
restore) restore "$@" ;;
import-backup)
	[[ $# -eq 0 ]] || usage
	import_backup
	;;
import-passphrase) import_passphrase "$@" ;;
recover) recover "$@" ;;
repair-data) repair_data "$@" ;;
resume) resume "$@" ;;
doctor) doctor ;;
check-isolation) check_isolation ;;
_seed) seed ;;
_install) exec timeout -k 10 "$INSTALL_TIMEOUT" "$0" _install_now ;;
_install_now) install_guest ;;
_backup) backup_now "$@" ;;
_shutdown) shut_down ;;
*) usage ;;
esac
