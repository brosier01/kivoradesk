# KivoraDesk

A self-hosted, persistent and isolated Linux computer that an AI agent can use
like a person does: look at the screen, move the mouse, type, browse the web,
run commands and keep files.

KivoraDesk runs an Alpine Linux VM on your own server (QEMU/KVM + libvirt).
Claude Code stays on your laptop and drives the VM through an MCP server reached
over SSH. You can watch the desktop live and take over at any time.

> **Status:** early development. Phase 0 (feasibility) is done; the MCP server
> and the live view are not built yet, so there is nothing to use day to day.

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
| 1 | Live view from the laptop, SSH access path, persistent volumes | Next |
| 2 | MCP server: `computer`, `browser`, `shell`, `files`, `human`; agent/human lock; LAN blocked | Planned |
| 3 | Robustness: separate data disk, update/recover/reset, encrypted backups, audit log, secrets, mobile view | Planned |
| 4 | Optimisation and published benchmarks | Planned |
| 5 | Installation guide and first release | Planned |

## Repository layout

```text
cmd/kivora-probe/   input accuracy probe, runs inside the VM
internal/cdp/       minimal Chrome DevTools Protocol client
image/cloud-init/   first-boot configuration (admin user, SSH key)
image/alpine/       guest provisioning script and OpenRC services
scripts/vm/         host-side scripts: create, provision, ssh, destroy
```

## Development VM

Requirements on the host: KVM, libvirt (`qemu:///system`, user in the
`libvirt` group), `virt-install`, `qemu-img`, `xorriso` and Go. No sudo is
needed: disks live in a libvirt-managed storage pool.

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
`:1` (1280x800), with services supervised by OpenRC (`kivora-display`,
`kivora-wm`, `kivora-browser`, `kivora-vnc`). Chromium DevTools (port 9222)
and VNC (port 5900) listen on localhost only; reach VNC through an SSH tunnel.
Use `virsh shutdown --mode agent kivoradesk` for a clean shutdown: the Alpine
virt image has no ACPI power button.

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
