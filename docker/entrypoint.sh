#!/bin/bash
# PID 1 of the container: run the VM, and shut it down cleanly when the container
# stops. The data stays in the volume, on the VM's disks.
#
# This script stays root, with SETUID and SETGID only, to start each part under
# its own account: the VM and its network as kivora-vm, the admin commands as
# kivora. Should the VM ever get out of QEMU, the SSH keys, the backups and their
# passphrase are out of its reach. Root here has no other right: it cannot enter
# these accounts' directories or signal their processes.
#
# When the VM cannot run or cannot be set up, the container stays up without
# restarting: the admin commands keep answering, and doctor says what to do.
set -euo pipefail

source /usr/local/bin/kivora --source

export VM_DISK_GB=${VM_DISK_GB:-16}
export VM_DATA_GB=${VM_DATA_GB:-16}
VM_VCPUS=${VM_VCPUS:-2}
VM_MEMORY_MB=${VM_MEMORY_MB:-4096}
VM_MAC=52:54:00:6b:64:01
# The network process holds what the VM sends it, about 2 GiB at worst. It may not
# reach the server's memory: past this address space it dies, and the container
# restarts.
NET_ADDRESS_SPACE_MB=4096
NET_MEMORY=2GiB

cd /

as_vm() { su-exec kivora-vm "$@"; }
as_admin() { su-exec kivora "$@"; }
alive() { [[ -n ${1:-} && -d /proc/$1 ]]; }

net="" qemu="" installer=""

stop_vm() {
	local code=${1:-0}
	trap '' TERM INT
	log "shutting the VM down"
	if alive "$installer"; then as_admin kill "$installer" 2>/dev/null || true; fi
	if alive "$qemu"; then
		kivora _shutdown >/dev/null 2>&1 &
		for _ in $(seq 45); do
			alive "$qemu" || break
			sleep 1
		done
		if alive "$qemu"; then as_vm kill "$qemu" 2>/dev/null || true; fi
	fi
	if alive "$net"; then as_vm kill "$net" 2>/dev/null || true; fi
	wait 2>/dev/null || true
	exit "$code"
}
trap stop_vm TERM INT

degraded() {
	log "$*"
	printf '%s\n' "$*" >"$DEGRADED"
}

requested() {
	as_vm sh -c 'cd "$1" && { [ -e data.restore.qcow2 ] || [ -e recover ] || [ -e restart ]; }' _ "$SHARED_DIR"
}

# What "kivora restore", "recover" and "repair-data" asked for, once the VM has
# stopped. A request is removed by the account that made it, the only one that can
# write there. The restored disk changes directory without being copied.
apply_requests() {
	rm -f "$DEGRADED"
	if as_vm test -e "$SHARED_DIR/data.restore.qcow2"; then
		log "putting the restored disk in place"
		if as_vm sh -eu -c '
			cd "$1"
			rm -f data.part
			ln "$2/data.restore.qcow2" data.part 2>/dev/null || { umask 007; cp "$2/data.restore.qcow2" data.part; }
			mv data.part data.qcow2
		' _ "$VM_DIR" "$SHARED_DIR"; then
			as_admin rm -f "$SHARED_DIR/data.restore.qcow2"
		else
			as_vm rm -f "$VM_DIR/data.part"
			as_admin mv "$SHARED_DIR/data.restore.qcow2" "$SHARED_DIR/restore.failed"
		fi
	fi
	if as_vm test -e "$SHARED_DIR/recover"; then
		as_vm rm -f "$VM_DIR/sys.qcow2"
		as_admin rm -f "$SHARED_DIR/recover"
	fi
	as_admin rm -f "$SHARED_DIR/restart"
}

prepare_disks() {
	as_vm test -f "$SHARED_DIR/seed.iso" || kivora _seed || return 1
	as_vm test -f "$VM_DIR/sys.qcow2" && return 0
	log "preparing the disks"
	kivora _seed || return 1
	as_vm sh -eu -c '
		umask 007
		cd "$1"
		rm -f sys.part
		cp /opt/kivoradesk/alpine.qcow2 sys.part
		qemu-img resize -q sys.part "${VM_DISK_GB}G"
		[ -f data.qcow2 ] || qemu-img create -q -f qcow2 data.qcow2 "${VM_DATA_GB}G"
		mv sys.part sys.qcow2
	' _ "$VM_DIR"
}

disk_opens() { as_vm qemu-img info -f qcow2 "$VM_DIR/$1" >/dev/null 2>&1; }

# QEMU would stop at once, and the container with it, without end.
check_disks() {
	if ! disk_opens data.qcow2; then
		degraded "the VM's data disk is missing or does not open: kivora restore --yes --no-backup brings back a backup"
		return 1
	fi
	if ! disk_opens sys.qcow2; then
		degraded "the VM's system disk does not open: kivora recover --yes rebuilds it and keeps the data"
		return 1
	fi
}

# The group of /dev/kvm depends on the host. QEMU has it and its own: it opens its
# files, those the admin commands leave it in their directory, and nothing else.
start_qemu() {
	log "starting the VM (${VM_VCPUS} vCPU, ${VM_MEMORY_MB} MiB)"
	as_vm rm -f "$VM_DIR/qga.sock" "$VM_DIR/qmp.sock"
	(
		umask 007
		exec setpriv --reuid=kivora-vm --regid=kivora-vm --groups="kivora-vm,$(stat -c %g /dev/kvm)" \
			qemu-system-x86_64 -name kivoradesk \
			-machine q35,accel=kvm -cpu host -smp "$VM_VCPUS" -m "$VM_MEMORY_MB" \
			-nodefaults -display none \
			-chardev "socket,id=console,path=$VM_DIR/console.sock" -serial chardev:console \
			-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny \
			-drive "file=$VM_DIR/sys.qcow2,if=none,id=sys,format=qcow2" -device virtio-blk-pci,drive=sys,bootindex=0 \
			-drive "file=$VM_DIR/data.qcow2,if=none,id=data,format=qcow2" -device virtio-blk-pci,drive=data,serial=kivora-data \
			-drive "file=$SHARED_DIR/seed.iso,if=none,id=seed,format=raw,readonly=on" -device virtio-blk-pci,drive=seed \
			-netdev "stream,id=n0,server=off,addr.type=unix,addr.path=$VM_DIR/net.sock" \
			-device virtio-net-pci,netdev=n0,mac="$VM_MAC",romfile= \
			-chardev "socket,id=qga,path=$VM_DIR/qga.sock,server=on,wait=off" \
			-device virtio-serial-pci -device virtserialport,chardev=qga,name=org.qemu.guest_agent.0 \
			-qmp "unix:$VM_DIR/qmp.sock,server=on,wait=off" \
			-device virtio-rng-pci
	) &
	qemu=$!
}

# No VM, and nothing to do until a human asks for something.
wait_for_request() {
	until requested; do
		alive "$net" || stop_vm 1
		sleep 2 &
		wait $! || true
	done
}

# What a stop in the middle of an operation left: a copy of a disk is in clear.
as_admin sh -c 'cd "$1" && rm -rf restore.* backups/*.part && cd "$2" && rm -f backup.qcow2 ./*.part ./*.new restore.failed' \
	_ "$ADMIN_DIR" "$SHARED_DIR"
as_vm rm -f "$VM_DIR/data.part" "$VM_DIR/sys.part"

# The network first: QEMU connects to it. It outlives the VM's restarts. Not
# through as_vm: a function in the background is a subshell, and $! its PID.
as_vm rm -f "$VM_DIR/net.sock" "$VM_DIR/console.sock"
(
	ulimit -v $((NET_ADDRESS_SPACE_MB * 1024))
	export GOMEMLIMIT=$NET_MEMORY
	exec su-exec kivora-vm kivora-net -hypervisor "unix:$VM_DIR/net.sock" -forward "0.0.0.0:$SSH_PORT=22" \
		-allowlist "$ALLOWLIST" -host-addresses "$HOST_ADDRESSES" \
		-console "$VM_DIR/console.sock=$VM_DIR/console.log"
) &
net=$!
for _ in $(seq 50); do
	as_vm test -S "$VM_DIR/net.sock" && break
	sleep 0.1
done

while :; do
	apply_requests
	if ! prepare_disks; then
		degraded "the VM's disks could not be prepared: is the volume full? See docker compose logs, then docker compose restart"
		wait_for_request
		continue
	fi
	if ! check_disks; then
		wait_for_request
		continue
	fi
	start_qemu
	started=$SECONDS

	kivora _install &
	installer=$!
	ended="" status=0
	wait -n -p ended "$installer" "$qemu" "$net" || status=$?
	if [[ $ended == "$installer" ]]; then
		installer=""
		if ((status == 0)); then
			: >"$READY"
			log "KivoraDesk is ready"
		else
			degraded "installing in the VM failed: see docker compose logs. A damaged data disk: kivora repair-data --yes. Otherwise fix the cause, then docker compose restart"
		fi
		ended=""
		wait -n -p ended "$qemu" "$net" || true
	else
		as_admin kill "$installer" 2>/dev/null || true
		installer=""
	fi
	rm -f "$READY"

	if [[ $ended == "$qemu" ]]; then
		if requested; then
			log "restarting the VM"
			continue
		fi
		if ((SECONDS - started < 15)); then
			degraded "the VM stopped as soon as it started: see docker compose logs, then docker compose restart"
			wait_for_request
			continue
		fi
	fi
	log "the VM or its network stopped: see console.log in the volume"
	stop_vm 1
done
