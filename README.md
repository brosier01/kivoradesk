# KivoraDesk

A persistent, isolated and self-hostable graphical Linux computer, designed to be driven by Claude Code.

KivoraDesk runs an Alpine Linux VM on a homelab host (QEMU/KVM + libvirt) with a
visible Chromium desktop, a durable `/workspace`, and a local MCP server exposing
reliable Computer Use primitives (screenshot, mouse, keyboard, windows). A human
can watch the desktop live, pause the agent and take over at any time.

> **Status:** early development (Phase 0 — feasibility). Nothing here is usable yet.

## Goals

- A real computer: a KVM guest separate from the host, with an X11 desktop and Chromium.
- Claude Code running inside the VM, with the MCP tools available.
- Reliable Computer Use, measured by a reproducible test suite.
- Persistence of files, browser profile and configuration across reboots.
- Human control: live view, instant pause, conflict-free takeover.
- Security: no VNC, CDP or MCP endpoint exposed publicly; least privilege.

## Non-goals (for now)

- Multi-tenant SaaS, orchestration of many VMs, billing.
- CAPTCHA solving or bypassing website restrictions.
- Autonomous root administration of the host.

## Development VM

Requirements on the host: KVM, libvirt (`qemu:///system`, user in the
`libvirt` group), `virt-install`, `qemu-img`, `xorriso` and Go.

```sh
scripts/vm/create.sh        # download Alpine, create the VM, wait for SSH
scripts/vm/provision.sh     # install the desktop and services in the VM
scripts/vm/ssh.sh           # open a shell as the admin user
scripts/vm/destroy.sh --yes # delete the VM and its disk
```

Settings (VM name, size, Alpine release, libvirt pool) can be overridden
through the environment; see `scripts/vm/common.sh`. Data that must stay out
of the repository (SSH key, image cache) lives in `~/.local/share/kivoradesk`.

Inside the VM, the desktop runs as the unprivileged `agent` user on display
`:1` (1280x800). Chromium DevTools (port 9222) and VNC (port 5900) listen on
localhost only; reach VNC through an SSH tunnel.

### Input accuracy probe

```sh
CGO_ENABLED=0 go build -o bin/kivora-probe ./cmd/kivora-probe
scripts/vm/ssh.sh 'cat > /tmp/kivora-probe && chmod 755 /tmp/kivora-probe' < bin/kivora-probe
scripts/vm/ssh.sh 'doas -u agent env DISPLAY=:1 /tmp/kivora-probe -n 1000'
```

## Claude Code

KivoraDesk does not modify or redistribute Claude Code. Installing and using
Claude Code is governed by Anthropic's terms; each user must authenticate with
their own authorized account.

## License

Apache-2.0 — see [LICENSE](LICENSE).
