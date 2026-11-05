# Changelog

All notable changes to KivoraDesk are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
break compatibility.

## 0.4.0 - 2026-11-05

First public release: an install reduced to `git clone` and `docker compose up`,
tested on three Linux distributions, and firmer boundaries between the web, the
agent and the server after an internal security audit.

### Upgrading from 0.3.0

The libvirt install is gone, along with `scripts/vm`. To keep your data, make a
last backup with `scripts/vm/backup.sh` while you still have them, then bring it to
the new install with `kivora import-passphrase`, `kivora import-backup` and
`kivora restore --yes`. Laptops need their SSH setup again: see `kivora add-laptop`.

### Added

- **Docker install.** `docker compose up -d --wait` builds the image, creates the VM
  inside the container and installs it. The server needs Docker and KVM, no longer
  libvirt or Go. `git pull` and `docker compose up -d --build` update it, Alpine
  packages included.
- **The `kivora` command**, run through `docker compose exec`, covers everything on
  the server: laptops, sign-in links, credentials, allowed sites and services,
  backups, restore, recovery and diagnostics.
- **`kivora add-laptop`** authorizes a laptop's key and prints its SSH setup, with the
  VM's host key: the first connection asks nothing, and works without a terminal.
- **Shell network.** Shell commands reach the Internet through `kivora-proxy` only:
  HTTPS tunnels to the sites listed with `kivora shell-allow`. The list starts empty
  and applies at once.
- **Sign-in links.** `kivora web-url` gives a code that works once, for 5 minutes, and
  opens a session of 30 days. `--rotate` signs out every browser.
- `SECURITY.md`: how to report a vulnerability, what KivoraDesk protects and what it
  does not.
- A GitHub Actions workflow for the build, `go vet`, the unit tests and the image.
  The VM tests need KVM and are not part of it.

### Changed

- **The VM's network** is a process, `kivora-net`, in which its frames end. It only
  opens TCP connections to the Internet and answers DNS. There is no other UDP, no
  ping and no IPv6. `kivora allow` exceptions are TCP only.
- Shell commands have no other network than the proxy: files from the web go through
  `browser_download`. Background jobs end with the call, and each call has limits on
  processes, memory per process and file size.
- The browser blocks local files, browser pages, the developer tools, extensions,
  file dialogs, its own downloads, the password manager and the VM's local service
  ports.
- Each credential has a kind of field. Password credentials, the default, go into
  password fields only; text ones also go into login, email and one-time code fields.
  Search boxes and text areas are always refused.
- The live view uses `kivora.localhost`, and the permanent token is gone.
- `scp` runs as the worker and starts in `/workspace`.
- A tab that does not answer, usually because of a dialog, gives an error after 3
  seconds rather than holding every browser tool for 2 minutes.
- `kivora-scenarios`, `kivora-bench` and the fault tests take the command that runs
  the admin commands: `-admin` or `KIVORA_ADMIN`.

### Removed

- The libvirt install: `scripts/vm` and its network filter. Docker is the only one.

### Security

- **The container.** QEMU and the network run under their own account, without
  capabilities, on a read-only root filesystem. The SSH keys, the backups and their
  passphrase are out of their reach.
- **The server's addresses.** The VM reaches no private network, and none of the
  server's addresses, public ones included: a one-shot service reads them at each `up`.
- **The server's disk.** The VM's console file and the container's logs are bounded.
- **Credentials** belong to a `kivora-vault` account: the desktop account may only
  list them and ask for one value at a time. Values filled in a session, and their
  common encodings, are removed from the text of every result and error.
- **The audit log** belongs to a `kivora-audit` account: the desktop account can add
  entries, not read, change or delete them. A call that could not be logged says so,
  and the MCP server does not start without its log.
- **Local services.** The worker reaches no local address, VNC is reserved to the
  gateway and DevTools to the agent and root. Each account only sees its own
  processes, and the X cookie and typed text are off command lines.
- **SSH.** The agent's keys are out of its reach in `/etc/ssh/authorized_keys`. The
  VM's host key is created before its first boot, so the admin commands refuse any
  other, and it survives `kivora recover`.
- **Web content.** All third-party text is marked as untrusted with a random tag per
  result, which the content cannot close. The scripts of the browser tools run in an
  isolated world, out of the page's reach.
- **`browser_download`** refuses the VM's own addresses, after name resolution and at
  each redirect, where cookies are looked up again.
- **Web gateway.** Every answer has the security headers, only the noVNC files the
  viewer loads are served, and the VM only stores hashes of codes and sessions.
- **Data.** A failed backup leaves no clear copy, and a restore refuses a disk naming
  another file. The data disk is mounted without setuid programs or device files.
- **Supply chain.** The Alpine image is checked against its signature, and the move
  to Go 1.27.3 ends 17 known vulnerabilities reachable from the code.

### Fixed

- A navigation that timed out on a reused connection kept the tab from answering,
  even once the network was back.
- The input owner survives restarts: a crash, an update or a reboot no longer hands
  the desktop back during a human takeover.
- A malformed clipboard length from a signed-in viewer could take the gateway down.
- Cleaning up after a failed download, the browser service and `provision.sh` no
  longer follow links left in writable directories.
- Chromium could exit on a new profile: the GPU shader disk cache is off.
- `kivora-bench` could no longer reach the browser since the firewall reserved DevTools.

### Measured

- Scenario suite: 300/300 runs on an install from scratch, on Ubuntu 24.04. All the
  integration tests pass, fault injection included.
- From blank servers, with their distribution's Docker packages: 30/30 scenarios on
  Debian 13 and on Fedora 44 with SELinux enforcing, with `kivora doctor` and
  `kivora check-isolation` clean on each, and backup, restore, recovery, restart and
  update.
- A laptop on another machine, through a published port and through `ProxyJump`.
- A real instance moved from libvirt to Docker, its files, keys, credentials and audit
  log identical.
- `govulncheck` finds no known vulnerabilities.

### Known limitations

- The server must run Linux on x86_64 with KVM and Docker. macOS, Windows, FreeBSD and
  ARM cannot host the VM for now.
- Without UDP, video calls in the VM's browser may fail (not tested).
- `docker compose down -v` deletes the data and the backups: copy them out first.
- The server's addresses are read at each `up`: a public address that changes in
  between is only known at the next one.
- The installation was tested on nested servers, not yet on a cloud server or with
  rootless Docker, and the laptop steps from Linux, not yet from macOS. The live view
  was tested over HTTP through the tunnel, not in a real browser, since the move to
  Docker.
- The browser is not limited to allowed sites: a page can still lead the agent to send
  out what it reads. Screenshots cannot be cleaned of filled credentials.
- A Chromium utility process that applies component updates keeps being stopped by its
  sandbox. The browser survives it.
- Closing the laptop still stops the agent; the VM, browser and files stay intact.

## 0.3.0 - 2026-11-01

Optimization: a lighter VM that boots in half the time, with a published
benchmark and the tool to reproduce it.

### Added

- **Benchmark.** `kivora-bench` reboots the VM and measures each boot stage,
  disk use, idle memory by component and CPU, the latency of the main MCP
  tools and memory with real sites open; `-compare` shows the change between
  two runs for A/B tests. Results and method in `docs/benchmark.md`.
- `KIVORA_BROWSER_FLAGS` in `/etc/conf.d/kivoradesk` for extra Chromium flags.

### Changed

- Reboot to working MCP tools: 29.5 s → 13.3 s. The boot menu waits 1 s
  instead of 10 s, and dhcpcd no longer probes for address conflicts (the
  address is reserved on the libvirt network).
- System disk: 1021 → 880 MiB, 453 → 315 packages. Only the noVNC web client
  is installed (no websockify, Python or NumPy), and cloud-init is removed
  once the first boot is done.
- Chromium starts with `--disable-gpu`: the VM has no GPU, and the emulated
  one cost about 50 MiB with no WebGL either way.
- `/tmp` is emptied at boot; Chromium left directories there after crashes.

### Fixed

- After a rebuild of the system disk (`recover.sh`) followed by a reboot, the
  KivoraDesk services could stay stopped: OpenRC kept a dependency cache
  written in the same second as the service scripts. The cache is now rebuilt
  after the services are installed.

### Measured

- Tool latency unchanged (for example screenshot 50 ms, navigation 38 ms,
  page reading 2 ms at the median).
- Scenario suite: 300/300 runs after the changes, on a rebuilt VM too.

### Known limitations

- Memory used at idle drops by 4 % only (598 → 572 MiB): Chromium is most of
  it.

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

