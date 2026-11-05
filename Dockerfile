# The container runs the VM: all of KivoraDesk stays inside the VM, and the
# container only brings QEMU, the VM's network and what the first start needs.

FROM golang:1.27.3-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN mkdir -p /out/guest \
	&& for cmd in kivora-web kivora-mcp kivora-vault kivora-audit kivora-proxy; do \
		CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "/out/guest/$cmd" "./cmd/$cmd" || exit 1; \
	done \
	&& for cmd in kivora-net kivora-vmctl; do \
		CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "/out/$cmd" "./cmd/$cmd" || exit 1; \
	done

# The Alpine cloud image, checked against its signature: a checksum from the same
# mirror would only detect a damaged download.
FROM alpine:3.24 AS base-image
ARG ALPINE_BRANCH=v3.24
ARG ALPINE_IMAGE=alpine-3.24.3-x86_64-cloudinit-r0.qcow2
RUN apk add --no-cache curl gnupg
COPY image/alpine/cloud-images-signing-key.asc /key.asc
RUN url=https://dl-cdn.alpinelinux.org/alpine/$ALPINE_BRANCH/releases/cloud/$ALPINE_IMAGE \
	&& curl --proto '=https' -fsSL -o /alpine.qcow2 "$url" \
	&& curl --proto '=https' -fsSL -o /alpine.asc "$url.asc" \
	&& gpg --batch --quiet --dearmor -o /keyring.gpg /key.asc \
	&& gpgv --quiet --keyring /keyring.gpg /alpine.asc /alpine.qcow2

FROM alpine:3.24
# kivora runs the admin commands and owns the SSH keys and the backups. kivora-vm
# runs QEMU and the network, and owns the disks. The admin may enter the VM's
# directory, not the other way around.
RUN apk add --no-cache bash gpg gpg-agent openssh-client-default qemu-img qemu-system-x86_64 su-exec xorriso \
	&& adduser -D -u 1000 -h /var/lib/kivoradesk/admin kivora \
	&& adduser -D -u 1001 -h /var/lib/kivoradesk/vm kivora-vm \
	&& addgroup kivora kivora-vm \
	&& install -d -m 700 -o kivora -g kivora /var/lib/kivoradesk/admin \
	&& install -d -m 2770 -o kivora-vm -g kivora-vm /var/lib/kivoradesk/vm \
	&& install -d -m 755 -o kivora -g kivora /var/lib/kivoradesk-host
COPY --from=base-image /alpine.qcow2 /opt/kivoradesk/alpine.qcow2
COPY --from=build /out/kivora-net /out/kivora-vmctl /usr/local/bin/
COPY docker/entrypoint.sh /usr/local/bin/kivora-entrypoint
COPY docker/kivora.sh /usr/local/bin/kivora
COPY docker/doctor-guest.sh /opt/kivoradesk/doctor-guest.sh
COPY image/cloud-init/user-data.tmpl /opt/kivoradesk/user-data.tmpl
COPY image/alpine /opt/kivoradesk/guest
COPY --from=build /out/guest /opt/kivoradesk/guest/rootfs/usr/local/bin
# What to install in the VM changes with this sum: the entrypoint compares it.
RUN cd /opt/kivoradesk/guest && find . -type f | sort | xargs sha256sum | sha256sum >/opt/kivoradesk/guest.sum
EXPOSE 2222
VOLUME /var/lib/kivoradesk /var/lib/kivoradesk-host
ENTRYPOINT ["/usr/local/bin/kivora-entrypoint"]
