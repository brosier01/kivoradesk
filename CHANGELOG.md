# Changelog

All notable changes to KivoraDesk are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
break compatibility.

## 0.2.0 - 2026-11-01

Robustness and day-to-day operation: the computer recovers from failures,
keeps its data safe and can sign in to sites without exposing passwords.

### Added

- **Credentials.** A store on the VM's data disk, readable only by the
  desktop account (not encrypted on the VM; backups are), where each
  credential is bound to the sites (HTTPS origins) where it may be used.
  `secrets_list` and `secrets_fill` let the agent sign in without the value
  ever reaching the model; `secret.sh` manages credentials from the server,
  and password fields are masked in `browser_read_page`.
- **Backups.** `backup.sh` saves the data disk into GPG-encrypted files while
  the VM runs (filesystem freeze, libvirt backup), `restore.sh` brings one
  back, `update.sh` upgrades the VM packages and KivoraDesk (with a backup
  first and a reboot when a new kernel is installed) and `recover.sh` rebuilds
  the system disk while keeping the data.
- **Diagnosis.** `doctor.sh` checks the VM, services, isolation, gateway and
  browser in one pass; `computer_get_state` warns when disk space runs low.
- **Network allowlist.** `allow.sh` opens chosen private services (address and
  port) through the network filter; `check-isolation.sh` reports them.
- **Mobile live view.** The web view adapts to phones.
- **Scenario suite.** `kivora-scenarios` runs 30 complete workflows (search,
  reading, forms, downloads, tabs, shell and files, takeover, credentials,
  browser crash, Internet loss) through the real MCP server and reports the
  success rate.

### Changed

- Every tool call has a time limit, and navigations stop after 45 s.
- The audit log is written per day, kept 90 days, with secrets redacted.
- Browser tools wait for Chromium to come back after a crash, and the
  browser is restarted without limit; other services allow 5 restarts a
  minute.
- Chromium is closed normally before it is stopped, so sessions and cookies
  survive restarts and updates.

### Fixed

- `computer_type` could lose an accented character (about 1 in 100 on a
  busy host): Chromium drops a paste when the clipboard changes before the
  paste is done. The clipboard is now kept until the target has fetched the
  text and settled, and a piece of a line holding accents is pasted whole.
- `computer_focus_window` took 15 s and could report success without
  activating the window; it now lists titled windows only and checks focus.
- `doctor.sh` no longer reports a failure right after a deployment while
  Chromium starts.

### Measured

- Scenario suite: 300/300 runs (30 scenarios, 10 runs each), goal 90 %.
- Accented typing through `computer_type`: 100/100 exact with the host
  under heavy load.
- Backup of the data disk in about 4 s (52 MiB), restore in about 35 s.

### Known limitations

- Closing the laptop still stops the agent; the VM, browser and files stay
  intact.
- Without Internet, a navigation can take up to 45 s to fail.
- Backups are manual (no schedule).

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

