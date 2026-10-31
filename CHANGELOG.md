# Changelog

All notable changes to KivoraDesk are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
break compatibility.

## 0.1.0 - 2026-10-31

First release: Claude Code on a laptop drives a persistent Alpine Linux VM on
a homelab server, while the human watches it live and can take over.

### Added

- **Development VM.** Scripts to create, provision, rebuild and destroy an
  Alpine Linux 3.24 VM on libvirt without sudo: verified cloud image, fixed
  address, separate data disk for `/home/agent` and `/workspace` that survives
  system rebuilds, clean shutdown and filesystem freeze through the QEMU guest
  agent.
- **Desktop.** Xvfb (1280x800), Openbox, Chromium and x11vnc as supervised
  OpenRC services under an unprivileged `agent` account, with an X cookie per
  start and a persistent browser profile.
- **Live view and takeover.** `kivora-web` gateway serving noVNC through an
  SSH tunnel, with a token in a strict same-site cookie, Host and Origin checks
  and token rotation. It owns the input state (agent, paused, human) and
  filters viewer input on the VNC stream, releasing held keys and buttons when
  control moves.
- **MCP server.** `kivora-mcp`, started over SSH (stdio), with 32 tools:
  desktop (screenshot, mouse, keyboard, windows), structured browser over CDP
  (navigate, read page, click, type, tabs, PDF, download with the browser's
  cookies), shell, files in `/workspace`, and human takeover requests. Acting
  tools require agent control and report control changes; content from the
  web, files and commands is marked as untrusted; every call is audited.
- **Privilege separation.** Shell commands and file operations run as a
  `worker` account with no access to the display, DevTools, VNC, the gateway
  or the control API (local nftables rules by UID, `doas` limited to the
  helper). Writes by the desktop account into `/workspace` refuse symlinks.
- **Network isolation.** A libvirt network filter applied on the host keeps
  the VM off private networks (host, LAN, VPNs) while it reaches the Internet;
  `check-isolation.sh` verifies it.
- **Tests and tools.** Unit tests, integration tests against the VM (gateway
  and end-to-end MCP scenario over SSH) and `kivora-probe`, which measures
  native input accuracy against an oracle page.

### Measured

- 1000/1000 clicks land on the exact target pixel; double click, right click,
  wheel, ASCII typing and shortcuts 100 %.
- Accented typing through `computer_type`: 100/100 exact (xdotool alone:
  about 10 %).
- About 313 MiB of RAM used at idle and 490 MiB with three real sites open;
  1 GiB of disk; a VM is created and provisioned in under a minute.

### Known limitations

- Closing the laptop stops the agent; the VM, browser and files stay intact.
- No secrets vault, backups, update/recover/reset commands or mobile layout
  yet (planned for Phase 3).
- The VM cannot reach any private network, with no allowlist yet.

