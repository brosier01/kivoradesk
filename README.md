# KivoraDesk

A self-hosted, persistent and isolated Linux computer that an AI agent can use
like a person does: look at the screen, move the mouse, type, browse the web,
run commands and keep files.

KivoraDesk runs an Alpine Linux VM on your own server (QEMU/KVM + libvirt).
Claude Code stays on your laptop and drives the VM through an MCP server reached
over SSH. You can watch the desktop live and take over at any time.

> **Status:** early development. Phases 0 and 1 are done: the VM, its live
> view and human takeover work from a laptop. The MCP server is not built yet,
> so an agent cannot drive the desktop yet.

## How it works

```text
Laptop                              Homelab server (Linux, KVM)
┌──────────────────────┐            ┌──────────────────────────────────────┐
│ Claude Code          │── SSH ────▶│ VM "kivoradesk" (Alpine Linux)       │
│  └ MCP client (stdio)│            │  ├ MCP server (planned)              │
│ Browser: live view   │── private ▶│  ├ Xvfb + Openbox, 1280x800          │
└──────────────────────┘   network  │  ├ Chromium, persistent profile      │
                                    │  ├ /workspace (durable files)        │
                                    │  └ VNC + CDP on localhost only       │
                                    └──────────────────────────────────────┘
```

- **The VM is only the computer.** No agent runs inside it, and any MCP client
  can drive it.
- **No control port is exposed.** The MCP server talks over SSH (stdio); VNC
  and the Chromium DevTools endpoint listen on localhost inside the VM.
- **One input owner at a time.** Either the agent or the human holds the mouse
  and keyboard; a human takeover pauses the agent.
- **State survives.** Files, browser sessions and settings persist across
  reboots. Closing the laptop stops the agent, not the VM.

## Planned MCP tools

| Family | Purpose |
|---|---|
| `computer.*` | Screenshots, mouse, keyboard, windows, on the real desktop coordinates |
| `browser.*` | Structured browsing over CDP: navigate, tabs, read page text, fill forms, download |
| `shell.*` | Run commands in the VM as the unprivileged `agent` user |
| `files.*` | Read and write `/workspace`, transfer files to and from the laptop |
| `human.*` | Hand control to the human for SSO, passkeys, CAPTCHAs or 2FA, then resume |
| `secrets.*` | Fill credentials by reference, without exposing values to the model |

Web content returned to the model is marked as untrusted, and tools declare MCP
annotations (read-only, destructive) so the client can ask for approval.

## Phase 0 results

Measured inside the VM with `kivora-probe` (Alpine 3.24, Chromium 155,
2 vCPU, 4 GiB RAM). Each action targets a random point; an oracle page loaded
over CDP checks the event type, button and exact screen coordinates.

| Action | Attempts | Success |
|---|---:|---:|
| Left click | 1000 | 100 % |
| Double click, right click, wheel | 100 each | 100 % |
| ASCII typing, Ctrl+A / Backspace | 100 / 25 | 100 % |
| Non-ASCII typing via xdotool (é, €, œ) | 100 | ~10 % |

Non-ASCII typing will be implemented natively in the MCP server (XTest with
keymap synchronisation, CDP `Input.insertText` in the browser).

Resources: about 313 MiB RAM used at idle, 490 MiB with three real sites open,
1 GiB of disk. A new VM is created and provisioned in under a minute.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Feasibility: KVM, Alpine, Chromium, CDP, native input | Done |
| 1 | Live view from the laptop, human takeover, SSH access path, data disk, network isolation | Done |
| 2 | MCP server: `computer`, `browser`, `shell`, `files`, `human`; agent/human lock | Next |
| 3 | Robustness: separate data disk, update/recover/reset, encrypted backups, audit log, secrets, mobile view | Planned |
| 4 | Optimisation and published benchmarks | Planned |
| 5 | Installation guide and first release | Planned |

## Repository layout

```text
cmd/kivora-probe/   input accuracy probe, runs inside the VM
cmd/kivora-web/     web gateway: live view, takeover, input control state
internal/control/   input owner state (agent, paused, human)
internal/rfb/       VNC client-message filter used by the gateway
internal/cdp/       minimal Chrome DevTools Protocol client
image/cloud-init/   first-boot configuration (admin user, SSH key)
image/alpine/       guest provisioning script and OpenRC services
scripts/vm/         host-side scripts: create, provision, ssh, destroy, checks
host/libvirt/       libvirt network filter isolating the VM
```

## Development VM

Requirements on the host: KVM, libvirt (`qemu:///system`, user in the
`libvirt` group), `virt-install`, `qemu-img`, `xorriso` and Go. No sudo is
needed: disks live in a libvirt-managed storage pool.

```sh
scripts/vm/create.sh                    # download Alpine, create the VM, wait for SSH
scripts/vm/provision.sh                 # build the gateway, install desktop and services
scripts/vm/ssh.sh                       # open a shell as the admin user
scripts/vm/destroy.sh --yes --keep-data # rebuild the system, keep /home/agent and /workspace
scripts/vm/destroy.sh --yes             # delete the VM and all its data
scripts/vm/check-isolation.sh           # VM reaches the Internet, not private networks
```

Settings (VM name, sizes, fixed IP, Alpine release, libvirt pool) can be
overridden through the environment; see `scripts/vm/common.sh`. The VM gets a
DHCP reservation (`192.168.122.50` by default) on the libvirt network. Data
that must stay out of the repository (SSH key, image cache) lives in
`~/.local/share/kivoradesk`.

Inside the VM:

- The desktop runs as the unprivileged `agent` user on display `:1`
  (1280x800). Only processes holding the agent's X cookie can connect.
- `/home/agent` and `/workspace` live on a separate data disk mounted at
  `/data`, so rebuilding the system disk keeps files and browser logins.
- Services are supervised by OpenRC: `kivora-display`, `kivora-wm`,
  `kivora-browser`, `kivora-vnc` and `kivora-web`.
- Chromium DevTools (9222), VNC (5900) and the web gateway (6080) listen on
  localhost only. Over SSH, the `agent` account may only forward to 6080.

The VM is kept off private networks by a libvirt network filter
(`host/libvirt/nwfilter-no-lan.xml`), applied on the host so that nothing
inside the VM can lift it. New connections to 10.0.0.0/8, 172.16.0.0/12,
192.168.0.0/16, 169.254.0.0/16 and 100.64.0.0/10 are dropped, except DNS and
DHCP on the libvirt gateway; the host can still open SSH sessions to the VM.
`check-isolation.sh` verifies this against the host's own addresses and the
LAN router.

Use `virsh shutdown --mode agent kivoradesk` for a clean shutdown: the Alpine
virt image has no ACPI power button.

## Live view from the laptop

The web gateway (`kivora-web`) streams the desktop with noVNC and decides who
owns the input: **agent**, **paused** or **human**. Viewer input only reaches
the desktop while the human owns it; switching away releases any held key or
button.

On the server:

```sh
scripts/vm/authorize-key.sh ~/laptop_id_ed25519.pub   # the laptop's public key
scripts/vm/ssh-config.sh 192.168.1.66                 # how the laptop reaches the server
scripts/vm/web-url.sh                                 # one-time sign-in URL
scripts/vm/web-url.sh --rotate                        # new token, signs out every browser
```

On the laptop, add the output of `ssh-config.sh` to `~/.ssh/config`, then:

```sh
ssh -N kivoradesk          # keeps the tunnel open
open "http://localhost:6080/?token=…"   # the URL from web-url.sh, once
```

The token is stored in a strict same-site cookie. The gateway also rejects
non-loopback `Host` headers and cross-origin control requests.

Gateway integration tests run through the tunnel:

```sh
KIVORA_WEB_URL=http://localhost:6080 \
KIVORA_WEB_TOKEN=$(scripts/vm/ssh.sh 'doas cat /var/lib/kivora-web/token') \
KIVORA_XDOTOOL="$PWD/scripts/vm/ssh.sh doas -u agent env DISPLAY=:1 xdotool" \
go test -tags integration ./cmd/kivora-web
```

### Input accuracy probe

```sh
CGO_ENABLED=0 go build -o bin/kivora-probe ./cmd/kivora-probe
scripts/vm/ssh.sh 'cat > /tmp/kivora-probe && chmod 755 /tmp/kivora-probe' < bin/kivora-probe
scripts/vm/ssh.sh 'doas -u agent env DISPLAY=:1 /tmp/kivora-probe -n 1000'
```

Run `kivora-probe -h` for options (`-seed` for reproducible runs, `-json`).

## Contributing

The project follows git flow (`main`, `develop`, `feature/*`, `release/*`,
`hotfix/*`) and [Conventional Commits](https://www.conventionalcommits.org/).

## Claude Code

KivoraDesk does not modify or redistribute Claude Code. Installing and using
Claude Code is governed by Anthropic's terms; each user must authenticate with
their own authorized account.

## License

Apache-2.0 — see [LICENSE](LICENSE).
