# Changelog

All notable changes to KivoraDesk are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
break compatibility.

## 0.4.2 - 2026-11-06

### Changed

- **README:** an animation shows how KivoraDesk works.

## 0.4.1 - 2026-11-05

### Changed

- **Shorter README.** The details moved to `docs/administration.md`,
  `docs/architecture.md` and `docs/development.md`.
- **The version comes from the git tags**: `v0.4.1` on a release,
  `v0.4.1-3-gabc1234` after it, `dev` without git history. `KIVORA_VERSION` still
  sets it.

## 0.4.0 - 2026-11-05

First public release: an install reduced to `git clone` and `docker compose up`, and
firmer boundaries between the web, the agent and the server.

Earlier versions were never published, and there is no upgrade path from them: the
libvirt install and its `scripts/vm` are gone. Install 0.4.0 anew as in the README.

### Added

- **Docker install.** `docker compose up -d --wait` builds the image, creates the VM
  inside the container and installs it. The server needs Docker and KVM, no longer
  libvirt or Go. `git pull` and `docker compose up -d --build` update it, Alpine
  packages included. A new VM starts on up-to-date packages.
- **The `kivora` command**, run through `docker compose exec`, covers everything on
  the server: laptops, sign-in links, credentials, allowed sites and services,
  backups, restore, recovery and diagnostics.
- **`kivora add-laptop`** authorizes a laptop's key and prints its SSH setup, with the
  VM's host key: the first connection asks nothing, and works without a terminal.
  **`kivora laptops`** lists the keys and removes one.
- **`kivora repair-data`** repairs the data disk's filesystem, **`kivora resume`**
  continues a VM stopped by a full volume, and **`kivora restore --no-backup`** restores
  when the current data cannot be backed up.
- **Shell network.** Shell commands reach the Internet through `kivora-proxy` only:
  HTTPS tunnels to the sites listed with `kivora shell-allow`. The list starts empty
  and applies at once.
- **Sign-in links.** `kivora web-url` gives a code that works once, for 5 minutes, and
  opens a session of 30 days. `--rotate` signs out every browser and cuts their
  connections. `--port` gives the link for a tunnel on another local port.
- **`KIVORA_PUBLIC_ADDRESSES`** lists the server's public addresses that are on none
  of its interfaces, for the VM to refuse them.
- `file` and `pdftotext` in the VM, to read what the browser tools save.
- `SECURITY.md`: how to report a vulnerability, what KivoraDesk protects and what it
  does not.
- A GitHub Actions workflow for the build, formatting, the module files, `go vet`, the
  unit tests with the race detector, known vulnerabilities, the shell scripts and the
  image, and Dependabot. The VM tests need KVM and are not part of it.
- Kivo, the project's mascot.

### Changed

- **The VM's network** is a process, `kivora-net`, in which its frames end. It only
  opens TCP connections to the Internet and answers DNS. There is no other UDP and no
  IPv6, and only its gateway answers a ping. `kivora allow` exceptions are TCP only.
- **A laptop's key opens no shell.** It starts the MCP server and file transfers, and
  forwards to the live view. Use `kivora ssh` on the server for diagnosis. `scp -O` is
  refused.
- Shell commands have no other network than the proxy: files from the web go through
  `browser_download`. Each call has its own PID, IPC and mount namespaces, with no
  capability. Background jobs, shared memory and its private `/tmp`, `/var/tmp` and
  `/dev/shm` end with the call, and each call has limits on processes, memory per
  process and file size.
- The browser blocks local files, browser pages, the developer tools, extensions,
  file dialogs, its own downloads and the password manager, and refuses navigations to
  the VM's local service ports. Openbox has no menu.
- Each credential has a kind of field. Password credentials, the default, go into
  password fields only; text ones also go into login, email and one-time code fields.
  Search boxes and text areas are always refused.
- The live view uses `kivora.localhost`, and the permanent token is gone. In the VM,
  the gateway moves to the privileged port 680; the laptop keeps 6080.
- `scp` runs as the worker and starts in `/workspace`.
- `kivora restore` installs what the image brings in the VM again.
- `kivora-scenarios`, `kivora-bench` and the fault tests take the command that runs
  the admin commands: `-admin` or `KIVORA_ADMIN`.
- `kivora-scenarios` has 41 scenarios: new ones check the boundaries, and `-destructive`
  adds those that restart the VM or damage its data disk.

### Removed

- The libvirt install: `scripts/vm` and its network filter. Docker is the only one.
- `backup.sh --install-cron`: schedule `kivora backup` yourself if you want it.
- The automatic backup before an update: run `kivora backup` first.

### Security

- **The container.** QEMU and the network run under their own account, without
  capabilities, on a read-only root filesystem. The SSH keys, the backups and their
  passphrase are out of their reach, and the admin commands follow no path these
  accounts control. The accounts' UIDs are out of the range of human accounts of the
  server. Memory and processes are limited.
- **The server's addresses.** The VM reaches no private network, none of the server's
  addresses and not its router: a one-shot service reads them at each `up`, and the VM's
  connections are refused if this list cannot be read. Azure's platform address and
  IBM Cloud's service network are refused too.
- **The server's disk.** The VM's console file and the container's logs are bounded.
- **Credentials** belong to a `kivora-vault` account: the desktop account may only
  list them, ask for one value at a time and have a text cleaned. All stored values,
  and their common encodings, are removed from the text of every result and error, as a
  safety net. Audit entries are cleaned too.
- **The audit log** belongs to a `kivora-audit` account: the desktop account can add
  entries, not read, change or delete them. A call that could not be logged says so,
  and the MCP server does not start without its log.
- **Local services.** The worker reaches no local address, VNC is reserved to the
  gateway and DevTools to the agent and root. Each account only sees its own
  processes, and the X cookie and typed text are off command lines. No account but the
  gateway can take its port.
- **SSH.** The agent's keys are out of its reach in `/etc/ssh/authorized_keys`. The
  VM's host key is created before its first boot, so the admin commands refuse any
  other, and it survives `kivora recover`.
- **Web content.** All third-party text, in errors too, is marked as untrusted with a
  random tag per result, which the content cannot close. The scripts of the browser
  tools run in an isolated world, out of the page's reach, and the tab they act on does
  not depend on the page.
- **`browser_download`** refuses the VM's own addresses, after name resolution and at
  each redirect, where cookies are looked up again.
- **Web gateway.** Every answer has the security headers, only the noVNC files the
  viewer loads are served, and the VM only stores hashes of codes and sessions. A
  session that ends or is revoked cuts its viewer.
- **Data.** A failed backup leaves no clear copy, and a restore refuses a disk naming
  another file. The data disk is mounted without setuid programs or device files.
- **Kernel.** `io_uring` is off, and shell commands cannot create namespaces.
- **Supply chain.** The Alpine image is checked against its signature, the base images
  are pinned by digest, and the move to Go 1.27.3 ends 17 known vulnerabilities
  reachable from the code.

### Fixed

- A navigation that timed out on a reused connection kept the tab from answering,
  even once the network was back.
- The input owner survives restarts and `kivora recover`: a crash, an update or a
  reboot no longer hands the desktop back during a human takeover.
- **"Pause agent" failed silently once the session had ended.** The page now says so,
  and how to sign in again.
- **`kivora recover` or an update could format a data disk that could be repaired.**
  The install now only formats an empty disk.
- `KIVORA_BACKUP_KEEP=0` deleted every backup, and a restore could remove the backup
  it restored. The value must be at least 1, and a restore removes nothing.
- A broken data disk left the container restarting, with no command available. It now
  stays up, so that `kivora doctor` and `kivora restore` answer.
- A few failed SSH logins locked everyone out, the admin commands and the health check
  included, since every client reaches the VM from the same address.
- A page with a dialog or a busy loop held `browser_navigate` and every action for 2
  minutes. It now gives an error within seconds.
- A FIFO in `/workspace` held the session forever, and cancelling a `shell_run` left
  its command running.
- Shared memory left by a command could stall the VM long after the command ended.
- Parallel screenshots could return a damaged image, `browser_type` could report text
  it had not typed, and `browser_click` could click what covers the element.
- `files_write` over its limit cut the session or truncated the file; `files_list` had
  no limit.
- Long file names could lose their extension or end in the middle of a character.
- `docker compose up --wait` could fail while a slow install went on, and a clone with
  a strict umask built an unusable image.
- Stopping the container without SSH killed the VM rather than shutting it down, and
  its exit code was always 0.
- `kivora secret set` trimmed spaces and truncated long values without saying so.
- `kivora allow add` accepted addresses that the network then ignored.
- A malformed clipboard length from a signed-in viewer could take the gateway down.
- A long request from the agent could hide the desktop in the live view.
- Cleaning up after a failed download, the browser service and `provision.sh` no
  longer follow links left in writable directories.
- Chromium could exit on a new profile: the GPU shader disk cache is off.
- `kivora-bench` could no longer reach the browser since the firewall reserved DevTools.

### Measured

On the final code, on 2026-11-05, from a clean clone on Ubuntu 24.04 with Docker 29.1,
on a disposable instance:

- Scenario suite: 380/380 runs, 10 of each of the 38 scenarios with `-admin`, and the
  3 destructive ones. `kivora doctor` and `kivora check-isolation` clean.
- All the integration tests pass, fault injection included.
- An update with a change in the VM, its data kept.
- The benchmark: 13.4 s from a reboot to working MCP tools, 585 MiB of memory at idle.
- `go test -race`, `go vet` and `gofmt` clean.

On an earlier state of this version, before these last fixes:

- From blank servers, with their distribution's Docker packages: 30/30 scenarios on
  Debian 13 and on Fedora 44 with SELinux enforcing, with `kivora doctor` and
  `kivora check-isolation` clean on each, and backup, restore, recovery, restart and
  update.
- A laptop on another machine, through a published port and through `ProxyJump`.
- `govulncheck` finds no known vulnerabilities.

The live view was tested through the tunnel in Chromium and Firefox on Linux, with the
real VNC server: takeover, clicks, typing and handing control back. This was done
before the last merge of the fixes, not on the final code.

### Known limitations

- The server must run Linux on x86_64 with KVM and Docker Engine 25 or later. macOS,
  Windows, FreeBSD and ARM cannot host the VM for now.
- Without UDP, video calls in the VM's browser may fail (not tested).
- `docker compose down -v` deletes the data and the backups: copy them out first.
- The server's addresses are read at each `up`: a public address that changes in
  between is only known at the next one. One on none of its interfaces, behind NAT,
  must be listed in `KIVORA_PUBLIC_ADDRESSES`.
- The final code was tested on Ubuntu 24.04 only: Debian 13 and Fedora 44 ran an earlier
  state of this version. The installation was not tested yet on a cloud server or with
  rootless Docker, and the laptop steps from Linux, not yet from macOS. The live view
  was not tested in Safari or on a real phone.
- The browser is not limited to allowed sites: a page can still lead the agent to send
  out what it reads. Screenshots cannot be cleaned of filled credentials, and a site can
  show a value in a form that the cleaning of text does not recognize.
- The browser tools do not reach inside iframes and shadow DOM.
- There is no quota on `/workspace` as a whole, only a reserve for the audit log, the
  credentials and the browser. A command can use the VM's memory while it runs.
- Nothing updates by itself: Chromium, the VM's packages and the container's QEMU and
  OpenSSH move when you update them.
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

