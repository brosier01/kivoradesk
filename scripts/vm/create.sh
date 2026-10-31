#!/usr/bin/env bash
# Create the KivoraDesk VM from the official Alpine cloud image.
set -euo pipefail
source "$(dirname "$0")/common.sh"

command -v xorriso >/dev/null || die "xorriso is required to build the cloud-init seed"
command -v qemu-img >/dev/null || die "qemu-img is required"

if virsh_ dominfo "$VM_NAME" >/dev/null 2>&1; then
	die "VM '$VM_NAME' already exists (scripts/vm/destroy.sh to remove it)"
fi

# SSH key dedicated to the VM, kept outside the repository.
if [[ ! -f $KIVORA_SSH_KEY ]]; then
	log "generating SSH key $KIVORA_SSH_KEY"
	mkdir -p "$(dirname "$KIVORA_SSH_KEY")"
	ssh-keygen -q -t ed25519 -N '' -C "kivoradesk-admin" -f "$KIVORA_SSH_KEY"
fi

# Base image, verified against the published SHA-512.
mkdir -p "$KIVORA_CACHE_DIR"
base=$KIVORA_CACHE_DIR/$ALPINE_IMAGE
url=$ALPINE_MIRROR/$ALPINE_BRANCH/releases/cloud/$ALPINE_IMAGE
if [[ ! -f $base ]]; then
	log "downloading $url"
	curl -fSL -o "$base.part" "$url"
	mv "$base.part" "$base"
fi
curl -fsSL -o "$base.sha512" "$url.sha512"
echo "$(cut -d' ' -f1 <"$base.sha512")  $base" | sha512sum --check --quiet ||
	die "checksum mismatch for $base"

# Storage pool owned by libvirt, so no sudo is needed on the host.
if ! virsh_ pool-info "$LIBVIRT_POOL" >/dev/null 2>&1; then
	log "defining storage pool $LIBVIRT_POOL at $LIBVIRT_POOL_DIR"
	virsh_ pool-define-as "$LIBVIRT_POOL" dir --target "$LIBVIRT_POOL_DIR" >/dev/null
	virsh_ pool-build "$LIBVIRT_POOL" >/dev/null
	virsh_ pool-autostart "$LIBVIRT_POOL" >/dev/null
fi
virsh_ pool-start "$LIBVIRT_POOL" >/dev/null 2>&1 || true

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# upload_volume NAME FORMAT FILE creates a pool volume holding FILE.
upload_volume() {
	local size
	size=$(stat -c %s "$3")
	virsh_ vol-create-as "$LIBVIRT_POOL" "$1" "$size" --format "$2" >/dev/null
	virsh_ vol-upload --pool "$LIBVIRT_POOL" "$1" "$3"
}

# The VM does not exist, so system and seed volumes left by an interrupted run
# are stale. The data volume is never touched here.
for vol in "$VM_NAME.qcow2" "$VM_NAME-seed.iso"; do
	virsh_ vol-delete --pool "$LIBVIRT_POOL" "$vol" >/dev/null 2>&1 || true
done

log "preparing ${VM_DISK_GB}G system disk"
cp "$base" "$work/disk.qcow2"
qemu-img resize -q "$work/disk.qcow2" "${VM_DISK_GB}G"
upload_volume "$VM_NAME.qcow2" qcow2 "$work/disk.qcow2"

log "building cloud-init seed"
sed -e "s|@HOSTNAME@|$VM_NAME|g" \
	-e "s|@ADMIN@|$VM_ADMIN|g" \
	-e "s|@SSH_PUBKEY@|$(cat "$KIVORA_SSH_KEY.pub")|g" \
	"$REPO_ROOT/image/cloud-init/user-data.tmpl" >"$work/user-data"
printf 'instance-id: %s-%s\nlocal-hostname: %s\n' "$VM_NAME" "$(date +%s)" "$VM_NAME" >"$work/meta-data"
xorriso -as mkisofs -quiet -output "$work/seed.iso" -volid cidata -joliet -rock \
	"$work/user-data" "$work/meta-data"
upload_volume "$VM_NAME-seed.iso" raw "$work/seed.iso"

# The data disk holds /home/agent and /workspace. It survives a rebuild of
# the system disk (scripts/vm/destroy.sh --keep-data).
if virsh_ vol-info --pool "$LIBVIRT_POOL" "$VM_NAME-data.qcow2" >/dev/null 2>&1; then
	log "reusing existing data disk"
else
	log "creating ${VM_DATA_GB}G data disk"
	virsh_ vol-create-as "$LIBVIRT_POOL" "$VM_NAME-data.qcow2" "${VM_DATA_GB}G" --format qcow2 >/dev/null
fi

log "defining network filter $NWFILTER"
define_nwfilter

log "reserving $VM_IP on network $LIBVIRT_NETWORK"
virsh_ net-update "$LIBVIRT_NETWORK" delete ip-dhcp-host "$(dhcp_host_xml)" --live --config >/dev/null 2>&1 || true
virsh_ net-update "$LIBVIRT_NETWORK" add-last ip-dhcp-host "$(dhcp_host_xml)" --live --config >/dev/null

log "creating VM $VM_NAME (${VM_VCPUS} vCPU, ${VM_MEMORY_MB} MiB)"
virt-install --connect "$LIBVIRT_URI" \
	--name "$VM_NAME" \
	--vcpus "$VM_VCPUS" \
	--memory "$VM_MEMORY_MB" \
	--osinfo linux2022 \
	--import \
	--disk "vol=$LIBVIRT_POOL/$VM_NAME.qcow2,bus=virtio" \
	--disk "vol=$LIBVIRT_POOL/$VM_NAME-data.qcow2,bus=virtio,serial=kivora-data" \
	--disk "vol=$LIBVIRT_POOL/$VM_NAME-seed.iso,device=cdrom" \
	--network "network=$LIBVIRT_NETWORK,model=virtio,mac=$VM_MAC,filterref.filter=$NWFILTER" \
	--channel unix,target.type=virtio,target.name=org.qemu.guest_agent.0 \
	--graphics none \
	--console pty,target_type=serial \
	--noautoconsole >/dev/null

log "waiting for SSH"
mapfile -t ssh_opts < <(vm_ssh_opts)
for _ in $(seq 60); do
	ip=$(vm_ip)
	if [[ -n $ip ]] && ssh "${ssh_opts[@]}" "$VM_ADMIN@$ip" true 2>/dev/null; then
		log "VM ready: ssh -i $KIVORA_SSH_KEY $VM_ADMIN@$ip"
		exit 0
	fi
	sleep 5
done
die "VM did not become reachable over SSH (virsh console $VM_NAME to debug)"
