#!/bin/bash
# PID 1 of the container: run the VM, and shut it down cleanly when the container
# stops. The data stays in the volume, on the VM's disks.
#
# This script stays root, with SETUID and SETGID only, to start each part under
# its own account: the VM and its network as kivora-vm, the admin commands as
# kivora. Should the VM ever get out of QEMU, the SSH keys, the backups and their
# passphrase are out of its reach. Root here has no other right: it cannot enter
# these accounts' directories or signal their processes.
set -euo pipefail

source /usr/local/bin/kivora --source

export VM_DISK_GB=${VM_DISK_GB:-16}
export VM_DATA_GB=${VM_DATA_GB:-16}
VM_VCPUS=${VM_VCPUS:-2}
VM_MEMORY_MB=${VM_MEMORY_MB:-4096}
VM_MAC=52:54:00:6b:64:01

cd /

as_vm() { su-exec kivora-vm "$@"; }
alive() { [[ -n ${1:-} && -d /proc/$1 ]]; }

net="" qemu=""

stop_vm() {
	log "shutting the VM down"
	if alive "$qemu"; then
		kivora ssh 'doas poweroff' >/dev/null 2>&1 || true
		for _ in $(seq 45); do
			alive "$qemu" || break
			sleep 1
		done
		if alive "$qemu"; then as_vm kill "$qemu" 2>/dev/null || true; fi
	fi
	if alive "$net"; then as_vm kill "$net" 2>/dev/null || true; fi
	wait 2>/dev/null || true
	exit 0
}
trap stop_vm TERM INT

# What "kivora restore" and "kivora recover" asked for, once the VM has stopped.
apply_requests() {
	as_vm sh -eu -c '
		cd "$1"
		if [ -f data.restore.qcow2 ]; then mv data.restore.qcow2 data.qcow2; fi
		if [ -f recover ]; then rm -f sys.qcow2 recover; fi
	' _ "$VM_DIR"
}

prepare_disks() {
	as_vm test -f "$VM_DIR/sys.qcow2" && return 0
	log "preparing the disks"
	kivora _seed
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

# The group of /dev/kvm depends on the host. Started with a group, a process only
# has that one: QEMU opens its own files, or those left open to it by the admin
# commands, in a directory that no other account can enter.
start_qemu() {
	log "starting the VM (${VM_VCPUS} vCPU, ${VM_MEMORY_MB} MiB)"
	as_vm rm -f "$VM_DIR/qga.sock" "$VM_DIR/qmp.sock"
	(
		umask 007
		exec su-exec "kivora-vm:$(stat -c %g /dev/kvm)" qemu-system-x86_64 -name kivoradesk \
			-machine q35,accel=kvm -cpu host -smp "$VM_VCPUS" -m "$VM_MEMORY_MB" \
			-nodefaults -display none \
			-chardev "socket,id=console,path=$VM_DIR/console.sock" -serial chardev:console \
			-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny \
			-drive "file=$VM_DIR/sys.qcow2,if=none,id=sys,format=qcow2" -device virtio-blk-pci,drive=sys,bootindex=0 \
			-drive "file=$VM_DIR/data.qcow2,if=none,id=data,format=qcow2" -device virtio-blk-pci,drive=data,serial=kivora-data \
			-drive "file=$VM_DIR/seed.iso,if=none,id=seed,format=raw,readonly=on" -device virtio-blk-pci,drive=seed \
			-netdev "stream,id=n0,server=off,addr.type=unix,addr.path=$VM_DIR/net.sock" \
			-device virtio-net-pci,netdev=n0,mac="$VM_MAC",romfile= \
			-chardev "socket,id=qga,path=$VM_DIR/qga.sock,server=on,wait=off" \
			-device virtio-serial-pci -device virtserialport,chardev=qga,name=org.qemu.guest_agent.0 \
			-qmp "unix:$VM_DIR/qmp.sock,server=on,wait=off" \
			-device virtio-rng-pci
	) &
	qemu=$!
}

# The network first: QEMU connects to it. It outlives the VM's restarts. Not
# through as_vm: a function in the background is a subshell, and $! its PID.
as_vm rm -f "$VM_DIR/net.sock" "$VM_DIR/console.sock"
su-exec kivora-vm kivora-net -hypervisor "unix:$VM_DIR/net.sock" -forward "0.0.0.0:$SSH_PORT=22" \
	-allowlist "$ALLOWLIST" -host-addresses "$HOST_ADDRESSES" \
	-console "$VM_DIR/console.sock=$VM_DIR/console.log" &
net=$!
for _ in $(seq 50); do
	as_vm test -S "$VM_DIR/net.sock" && break
	sleep 0.1
done

while :; do
	apply_requests
	prepare_disks
	start_qemu

	kivora _install &
	installer=$!
	ended="" status=0
	wait -n -p ended "$installer" "$qemu" "$net" || status=$?
	if [[ $ended == "$installer" ]]; then
		if ((status != 0)); then
			log "installing in the VM failed"
			stop_vm
		fi
		: >/tmp/kivora-ready
		log "KivoraDesk is ready"
		ended=""
		wait -n -p ended "$qemu" "$net" || true
	else
		su-exec kivora kill "$installer" 2>/dev/null || true
	fi
	rm -f /tmp/kivora-ready

	if [[ $ended == "$qemu" ]] && as_vm sh -c 'cd "$1" && { [ -f data.restore.qcow2 ] || [ -f recover ]; }' _ "$VM_DIR"; then
		log "restarting the VM"
		continue
	fi
	log "the VM or its network stopped: see console.log in the volume"
	stop_vm
	exit 1
done
