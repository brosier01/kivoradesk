#!/bin/bash
# Administer the VM from inside the container:
#
#   docker compose exec kivoradesk kivora COMMAND
#
# Run it without a command for the list.
STATE=/var/lib/kivoradesk
ADMIN_DIR=$STATE/admin
VM_DIR=$STATE/vm
HOST_ADDRESSES=/var/lib/kivoradesk-host/addresses
KEY=$ADMIN_DIR/id_ed25519
HOST_KEY=$ADMIN_DIR/vm_host_ed25519
KNOWN_HOSTS=$ADMIN_DIR/known_hosts
PASSPHRASE=$ADMIN_DIR/backup.passphrase
BACKUPS=$ADMIN_DIR/backups
PROVISIONED=$ADMIN_DIR/provisioned
ALLOWLIST=$VM_DIR/allowlist
READY=/tmp/kivora-ready
ADMIN=kivora
AGENT=agent
SSH_PORT=2222
WEB_PORT=6080
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
admin_ssh() {
	ssh -p "$SSH_PORT" -i "$KEY" -F /dev/null -o UserKnownHostsFile="$KNOWN_HOSTS" -o StrictHostKeyChecking=yes \
		-o BatchMode=yes -o LogLevel=ERROR -o ConnectTimeout=5 "$ADMIN@127.0.0.1" "$@"
}

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
	# In the VM's directory, which only QEMU and this account can enter: QEMU, the
	# only reader, holds this key anyway through the disk it boots.
	xorriso -as mkisofs -quiet -output "$VM_DIR/seed.part" -volid cidata -joliet -rock \
		"$work/user-data" "$work/meta-data" "$work/network-config" 2>/dev/null
	chmod 644 "$VM_DIR/seed.part"
	mv "$VM_DIR/seed.part" "$VM_DIR/seed.iso"
	rm -f "$PROVISIONED"
}

wait_for_ssh() {
	for _ in $(seq 100); do
		admin_ssh true >/dev/null 2>&1 && return 0
		sleep 3
	done
	die "the VM did not become reachable over SSH"
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
	if ((!first_boot)); then
		log "upgrading the Alpine packages"
		admin_ssh 'doas apk update -q && doas apk upgrade -q --available'
	fi
	log "installing KivoraDesk in the VM"
	tar -C /opt/kivoradesk/guest -cf - . | admin_ssh '
		set -e
		dir=$(mktemp -d)
		trap "doas rm -rf $dir" EXIT
		tar -C "$dir" -xf -
		cd "$dir" && doas sh ./provision.sh
	'
	printf '%s\n' "$want" >"$PROVISIONED"
	# Alpine removes the running kernel's modules when it installs a new kernel.
	if ! admin_ssh '[ -d "/lib/modules/$(uname -r)" ]'; then
		log "new kernel installed: restarting the VM"
		admin_ssh 'doas reboot' >/dev/null 2>&1 || true
		sleep 5
		wait_for_ssh
	fi
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
	local key=${1:-} address=${2:-127.0.0.1}
	[[ $# -le 2 && $key != *$'\n'* ]] || die "$usage"
	[[ $key == ssh-* || $key == ecdsa-* || $key == sk-* ]] || die "$usage"
	printf '%s\n' "$key" | ssh-keygen -l -f - >/dev/null 2>&1 || die "this is not a valid public key"
	[[ $address =~ ^[A-Za-z0-9.:-]+$ ]] || die "invalid address: $address"
	authorize_key "$key"
	if [[ ${KIVORA_BIND:-127.0.0.1} == 127.* && $address != 127.* && $address != localhost ]]; then
		log "warning: the port is only published on this server: set KIVORA_BIND in .env (see the README), or add a ProxyJump"
	fi
	log "key authorized. Now run this on the laptop:"
	cat <<-EOF

		cat >>~/.ssh/config <<'CONFIG'

		# KivoraDesk. "ssh -N kivoradesk" opens the live view on http://kivora.localhost:$WEB_PORT
		Host kivoradesk
		    HostName $address
		    Port ${KIVORA_SSH_PORT:-$SSH_PORT}
		    User $AGENT
		    HostKeyAlias kivoradesk
		    LocalForward $WEB_PORT 127.0.0.1:$WEB_PORT
		    ServerAliveInterval 30
		CONFIG
		echo 'kivoradesk $(cut -d' ' -f1,2 "$HOST_KEY.pub")' >>~/.ssh/known_hosts
	EOF
}

web_url() {
	case ${1:-} in
	"") ;;
	--rotate)
		admin_ssh 'doas -u kivora-web /usr/local/bin/kivora-web sign-out' || die "could not sign out the browsers"
		log "every browser must sign in again"
		;;
	*) die "usage: kivora web-url [--rotate]" ;;
	esac
	local code
	code=$(admin_ssh 'doas -u kivora-web /usr/local/bin/kivora-web login-code') || die "could not get a sign-in code"
	echo "http://kivora.localhost:$WEB_PORT/login?code=$code"
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
		read -rsp "Value for $1 (not shown): " value || true
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

valid_allow_entry() {
	[[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})(/([0-9]|[12][0-9]|3[0-2]))?(:([0-9]{1,5}))?$ ]] || return 1
	local i
	for i in 1 2 3 4; do
		((10#${BASH_REMATCH[i]} <= 255)) || return 1
	done
	[[ -z ${BASH_REMATCH[8]} ]] || ((10#${BASH_REMATCH[8]} >= 1 && 10#${BASH_REMATCH[8]} <= 65535))
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
	state=$(vmctl status 2>/dev/null || true)
	if [[ $state != running ]]; then
		fail "VM is ${state:-not answering (or a backup is running: QEMU takes one client)}"
		return 1
	fi
	ok "VM is running"
	if [[ -f $HOST_ADDRESSES ]]; then
		ok "the VM's network knows $(grep -c ' address$' "$HOST_ADDRESSES" || true) address(es) of this server"
	else
		warn "addresses of this server unknown: if it has a public one, the VM can reach it (run docker compose up)"
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
	local failed=0 targets=() always=() target host port router="" gateway="" addr kind
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

# The caller holds the lock.
backup_now() {
	if [[ ! -f $PASSPHRASE ]]; then
		head -c 32 /dev/urandom | base64 >"$PASSPHRASE"
		log "created the passphrase of the backups: copy it elsewhere, they cannot be restored without it"
	fi
	mkdir -p "$BACKUPS"
	local out copy size old
	out=$(new_backup_path)
	copy=$VM_DIR/backup.qcow2
	cleanup_paths+=("$copy" "$out.part")
	gpg_home

	# Only the disk's line starts there: those of the file under it are indented.
	size=$(qemu-img info -U "$VM_DIR/data.qcow2" | sed -n 's/^virtual size: .*(\([0-9]*\) bytes)$/\1/p')
	[[ -n $size ]] || die "could not read the size of the data disk"
	rm -f "$copy"
	qemu-img create -q -f qcow2 "$copy" "$size"
	chmod 666 "$copy" # QEMU writes it, in a directory this account shares with QEMU only
	log "copying the data disk"
	vmctl backup data /data "$copy"
	log "encrypting into $out"
	# 9>&-: the agent gpg leaves running would keep the lock.
	gpg --batch --yes --quiet --pinentry-mode loopback --passphrase-file "$PASSPHRASE" \
		--symmetric --cipher-algo AES256 --compress-level 1 -o "$out.part" "$copy" 9>&-
	mv "$out.part" "$out"
	rm -f "$copy"

	mapfile -t old < <(list_backups | tail -n +$((${KIVORA_BACKUP_KEEP:-7} + 1)))
	((${#old[@]} == 0)) || {
		rm -f -- "${old[@]}"
		log "removed ${#old[@]} old backup(s)"
	}
	log "backup done: $out ($(du -h "$out" | cut -f1))"
}

# Stop the VM after a request to the entrypoint, and wait for it to come back.
restart_vm() {
	admin_ssh 'doas poweroff' >/dev/null 2>&1 || true
	local i
	for i in $(seq 60); do
		[[ -f $READY ]] || break
		sleep 1
	done
	if [[ -f $READY ]]; then
		log "the VM does not stop: forcing it"
		vmctl quit || true
	fi
	for i in $(seq 300); do
		[[ ! -f $READY ]] || return 0
		sleep 1
	done
	die "the VM did not come back: see docker compose logs"
}

restore() {
	if [[ ${1:-} == --list ]]; then
		list_backups || log "no backup"
		return 0
	fi
	[[ ${1:-} == --yes ]] || die "this replaces /home/agent and /workspace in the VM; re-run with --yes [BACKUP_FILE]"
	lock
	local file=${2:-}
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

	log "backing up the current data first"
	backup_now
	chmod 666 "$work/data.qcow2" # QEMU opens it, see backup_now
	mv "$work/data.qcow2" "$VM_DIR/data.restore.qcow2"
	log "restarting the VM on the restored data"
	restart_vm
	log "restored $file"
	doctor >/dev/null || log "warning: kivora doctor reports problems"
}

recover() {
	[[ ${1:-} == --yes ]] || die "this rebuilds the VM's system (data is kept); re-run with --yes"
	lock
	: >"$VM_DIR/recover"
	log "rebuilding the VM's system"
	restart_vm
	doctor
	log "recovered; browsers must sign in again: kivora web-url"
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
		  web-url [--rotate]                          sign-in link of the live view
		  secret set [--field text] NAME ORIGIN...    store a credential (asks for its value)
		  secret list | delete NAME
		  shell-allow add|remove HOST[:PORT] | list   sites the agent's shell may reach
		  allow add|remove IP[/BITS][:PORT] | list    private services the VM may reach
		  backup                                      encrypted backup of the data disk
		  restore --list | --yes [FILE]               reset the data to a backup
		  import-backup < FILE                        bring a backup from elsewhere
		  import-passphrase [--replace] < FILE        bring its passphrase
		  recover --yes                               rebuild a broken system, keep the data
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
		ssh -t -p "$SSH_PORT" -i "$KEY" -F /dev/null -o UserKnownHostsFile="$KNOWN_HOSTS" -o StrictHostKeyChecking=yes \
			-o LogLevel=ERROR "$ADMIN@127.0.0.1" "$@"
	else
		admin_ssh "$@"
	fi
	;;
health)
	# Healthy once installed, so that "docker compose up --wait" means ready.
	[[ -f $READY ]] && admin_ssh true
	;;
add-laptop) add_laptop "$@" ;;
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
doctor) doctor ;;
check-isolation) check_isolation ;;
_seed) seed ;;
_install) install_guest ;;
*) usage ;;
esac
