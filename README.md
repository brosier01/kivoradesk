# KivoraDesk

<p align="center">
  <img src="docs/assets/kivo.png" width="256" alt="Kivo, the KivoraDesk marmot, browsing the web from a desk in its burrow">
</p>

A self-hosted, persistent and isolated Linux computer that an AI agent can use
like a person does: look at the screen, move the mouse, type, browse the web,
run commands and keep files.

KivoraDesk runs an Alpine Linux VM on your own server (QEMU/KVM, in a Docker
container). Claude Code stays on your laptop and drives the VM through an MCP server reached
over SSH. You can watch the desktop live and take over at any time.

> **Status:** early development, first public release. Claude Code on a laptop
> drives the VM through the MCP server (web research, forms, PDFs, shell, files,
> credentials) while the human watches and can take over, with backups, recovery,
> a 30-scenario reliability suite at 100 % and a published
> [benchmark](docs/benchmark.md).

## How it works

```text
Laptop                              Server (Linux x86_64, KVM)
┌──────────────────────┐            ┌──────────────────────────────────────┐
│ Claude Code          │── SSH ────▶│ VM "kivoradesk" (Alpine Linux)       │
│  └ MCP client (stdio)│            │  ├ MCP server (kivora-mcp)           │
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

## Install

**The server** hosts the VM. It needs Linux on x86_64 with KVM (a cloud server must
expose it), Docker with Compose, 2 vCPU, 4 GiB of memory for the VM and up to 32 GiB
of disk, plus the backups. Tested on Ubuntu 24.04, Debian 13 and Fedora 44 with SELinux
enforcing, with their own Docker packages. macOS, Windows, FreeBSD and ARM cannot host
the VM for now.

**Your laptop** drives it. It needs OpenSSH and an MCP client such as Claude Code,
on any system. It can also be the server itself.

### On the server

```sh
git clone https://github.com/brosier01/kivoradesk.git
cd kivoradesk
docker compose up -d --wait
```

The first time takes a few minutes: it builds the image, with Alpine checked against
its signature, creates the VM inside the container and installs everything in it. If
Docker says `/dev/kvm` does not exist, KVM is not available on this machine: turn on
virtualization in its firmware settings, or choose a cloud server that exposes it.
Then check the result:

```sh
docker compose exec kivoradesk kivora doctor            # every line should start with ok
docker compose exec kivoradesk kivora check-isolation   # the VM reaches the Internet, not your network
```

From here on, `kivora COMMAND` stands for `docker compose exec kivoradesk kivora COMMAND`,
run on the server from this directory. `kivora` alone lists the commands.

### On your laptop

On the laptop, print your public key (run `ssh-keygen -t ed25519` first if you have
none):

```sh
cat ~/.ssh/id_ed25519.pub
```

On the server, authorize it by pasting it between the quotes. If the laptop is the
server itself, no address is needed:

```sh
kivora add-laptop "ssh-ed25519 AAAA... you@laptop"
```

If it is another machine, the VM's SSH port must be reachable from it. By default it
is only published on the server's `127.0.0.1:2222`. Choose one way:

- **Publish it on a LAN or VPN address of the server.** Put this in a `.env` file next
  to `compose.yaml`, run `docker compose up -d`, and give that address to `add-laptop`.
  Docker's published ports bypass ufw and firewalld: with `0.0.0.0` on a public server,
  the VM's SSH is open to the Internet, with keys only.

  ```sh
  KIVORA_BIND=192.168.1.10
  ```

  ```sh
  kivora add-laptop "ssh-ed25519 AAAA... you@laptop" 192.168.1.10
  ```

- **Keep it on `127.0.0.1` and go through your account on the server.** Run `add-laptop`
  without an address, and add `ProxyJump you@server` to the block it prints. An agent on
  the laptop then inherits what that account can do on the server.

`add-laptop` prints a short snippet: run it on the laptop. It adds the `kivoradesk`
host to `~/.ssh/config` and the VM's host key to `~/.ssh/known_hosts`, so the first
connection asks nothing. On a system without a POSIX shell, add these lines to the two
files by hand.

Then register the MCP server with Claude Code:

```sh
claude mcp add --scope user kivoradesk -- ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp
```

Ask Claude Code, for example, to research a topic on the web and save the sources
as PDFs in `/workspace`. To watch it, see [Live view](#live-view-from-the-laptop).

## MCP tools

| Family | Tools |
|---|---|
| Desktop | `computer_screenshot`, `computer_click`, `computer_move`, `computer_drag`, `computer_scroll`, `computer_type`, `computer_key`, `computer_windows`, `computer_focus_window`, `computer_get_state` |
| Browser (CDP, visible tab) | `browser_navigate`, `browser_read_page`, `browser_click`, `browser_type`, `browser_wait_for`, `browser_back`, `browser_forward`, `browser_reload`, `browser_tabs`, `browser_new_tab`, `browser_switch_tab`, `browser_close_tab`, `browser_save_pdf`, `browser_download` |
| Shell | `shell_run` |
| Files (`/workspace`) | `files_list`, `files_read`, `files_write`, `files_delete` |
| Credentials | `secrets_list`, `secrets_fill` |
| Human | `human_request_takeover`, `human_wait_for_control` |

- **Input lock.** Acting tools run only while the agent owns the desktop, one
  at a time, and report when control moved during the action. Observing tools
  are refused while the human uses the desktop. Only the human gives control
  back; the agent can ask for help, which pauses it and shows the reason in the
  live view.
- **Privilege separation.** The MCP server runs as `agent`, which owns the
  display and the browser. `shell_run` and `files_*` run as `worker`, which has
  no access to the display, any service of the VM, including SSH on its own
  address, or the control API (local firewall by UID, `doas` limited to the
  helper). Each account only sees its own processes. VNC is reserved to the
  gateway and DevTools to `agent`. Code the agent runs cannot drive the desktop
  behind the lock.
- **Untrusted content.** Page text, titles, addresses, file names and content,
  window names and command output are wrapped as untrusted data, in a tag with a
  random ID that the content cannot close. Screenshots cannot be wrapped. Tools
  declare MCP annotations (read-only, destructive).
- **Browser tools.** Their scripts run in an isolated world: a page cannot change
  what they read, pretend to be the visible tab or move the numbers of its
  elements. `browser_download` refuses the addresses of the VM itself.
- **Shell network.** Shell commands have no network of their own, not even DNS.
  Their only way out is a proxy reaching, over HTTPS, the sites you allow with
  `kivora shell-allow`, and none at first. It logs what it opens and refuses
  in `/var/log/kivora-proxy.log`. `browser_download` is how files get in. The browser
  is not limited: the agent can still send to a site what it reads elsewhere.
- **Shell limits.** Nothing `shell_run` starts outlives the call, even detached
  (each call has its own PID namespace, which needs unprivileged user namespaces).
  512 processes, 3 GiB per process and 8 GiB per file at most, low priority, and
  the first killed when memory runs out.
- **Typing.** `browser_type` inserts any text exactly through CDP.
  `computer_type` types ASCII with the keyboard and pastes other characters
  through the clipboard, checking control between chunks.
- **Audit.** Every call is logged as a JSON line with a trace ID in
  `/var/lib/kivora-audit/audit-YYYY-MM-DD.jsonl` on the data disk, kept 90 days,
  under an account of its own: the agent's side can add entries, not read, change
  or delete them. A call that could not be logged says so in its result.
  Passwords, tokens and keys are masked in the logged arguments.
- **Timeouts and recovery.** Every call has a maximum duration. After a
  Chromium crash, browser tools wait for the service to restart it; without
  Internet they fail at once with the browser's network error.
- **Credentials.** The human stores credentials with `kivora secret`,
  each bound to the sites where it may be used and to a kind of field. They live
  under a separate account that the browser cannot read. The agent sees names,
  sites and field kinds only; `secrets_fill` checks the page origin and the field
  and fills it in one step, and keeps it masked on screen. Filled values are
  removed from the text of every tool result. Screenshots cannot be cleaned: a
  site that displays a value it received shows it on screen. Once logged in, the
  agent can still read what the site itself displays.
- **Browser lockdown.** Chromium policies block local files, browser pages,
  the developer tools, extensions, file dialogs, downloads from the browser and
  its password manager, for the agent and for the human during a takeover. To
  change them, edit `/etc/chromium/policies/managed/kivora.json` in the VM.
  Tools refuse to read anything but web pages. `browser_download` and
  `browser_save_pdf` are the way to get files.
- **Files to the laptop.** Copy them with `scp kivoradesk:/workspace/<path> .`
  Transfers run as `worker`, in `/workspace`: a link left there cannot lead to the
  agent's private files. `scp -O`, the old protocol, does not get this protection.

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

The MCP server avoids that limit: `computer_type` pastes non-ASCII text
through the clipboard (100/100 exact in the end-to-end test) and
`browser_type` inserts text through CDP.

Resources: see the [benchmark](docs/benchmark.md). In short, with 2 vCPU and
4 GiB: 880 MiB of system disk, about 570 MiB of memory used at idle and
870 MiB with three real sites open, CPU near zero at idle, and 13 s from a
reboot to working MCP tools. A new VM is created and provisioned in under a
minute.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Feasibility: KVM, Alpine, Chromium, CDP, native input | Done |
| 1 | Live view from the laptop, human takeover, SSH access path, data disk, network isolation | Done |
| 2 | MCP server: `computer`, `browser`, `shell`, `files`, `human`; agent/human lock; worker account | Done |
| 3 | Robustness: fault recovery, timeouts, diagnostics, update/recover/reset, encrypted backups, audit retention, credentials, mobile view, network allowlist, scenario suite | Done |
| 4 | Optimisation and published benchmarks | Done |
| 5 | Installation guide and first public release | Done |

## Repository layout

```text
cmd/kivora-audit/   audit log writer, run as its own account
cmd/kivora-bench/   resource and latency benchmark, A/B comparison
cmd/kivora-mcp/     MCP server (and the worker helper it runs as worker)
cmd/kivora-net/     the VM's network, in the container
cmd/kivora-probe/   input accuracy probe, runs inside the VM
cmd/kivora-proxy/   the only way out for shell commands, to the allowed sites
cmd/kivora-scenarios/ workflow reliability suite over MCP
cmd/kivora-vault/   credentials store, run as its own account
cmd/kivora-vmctl/   backups and status through QEMU and the guest agent, in the container
cmd/kivora-web/     web gateway: live view, takeover, input control state
internal/audit/     audit log files and the client of their writer
internal/browser/   structured browser tools over CDP
internal/control/   input owner state (agent, paused, human) and agent API
internal/desktop/   native mouse, keyboard, screenshots and windows
internal/secrets/   credentials bound to sites and kinds of field
internal/worker/    shell commands and file operations as the worker
internal/workspace/ symlink-safe writes into /workspace
internal/netguard/  refuses connections to this computer's own services
internal/relay/     joins two connections
internal/rfb/       VNC client-message filter used by the gateway
internal/cdp/       minimal Chrome DevTools Protocol client
internal/usernet/   TCP/IP stack where the VM's frames end, Internet only
image/cloud-init/   first-boot configuration (admin user, SSH key)
image/alpine/       guest provisioning script and OpenRC services
docker/             the container's entrypoint, kivora command and guest checks
Dockerfile          the image: QEMU, the VM's network, what the first start needs
compose.yaml        the install: the container, its rights, its volumes
```

## The VM

```sh
kivora ssh                                     # open a shell as the admin user
kivora doctor                                  # diagnose the VM, its services and the MCP server
kivora check-isolation                         # VM reaches the Internet, not private networks
kivora allow add 192.168.1.20:8080 staging     # let the VM reach one private service
kivora shell-allow add pypi.org                # let shell commands reach one site
docker compose down                            # shut the VM down, keep everything
docker compose down -v                         # delete the VM and all its data, backups included
```

The VM stops with the container and restarts with Docker (`restart: unless-stopped`):
it comes back when Docker starts with the server, unless you stopped it yourself.
Stopping the container shuts the VM down cleanly.

Settings go in a `.env` file next to `compose.yaml`. Run `docker compose up -d` after
changing them.

| Setting | Default | |
|---|---|---|
| `VM_VCPUS` | 2 | vCPUs of the VM |
| `VM_MEMORY_MB` | 4096 | memory of the VM |
| `VM_DISK_GB` | 16 | system disk, when it is created |
| `VM_DATA_GB` | 16 | data disk, when it is created |
| `KIVORA_BIND` | 127.0.0.1 | address of the server where the VM's SSH is published |
| `KIVORA_SSH_PORT` | 2222 | port of the server where the VM's SSH is published |
| `KIVORA_BACKUP_KEEP` | 7 | backups kept |

**Limits.** Without UDP, video calls in the VM's browser may fail (untested).

Inside the VM:

- The data disk is mounted without setuid programs or device files, and
  `/etc/sysctl.d/kivoradesk.conf` hardens the kernel for local accounts.
- The desktop runs as the unprivileged `agent` user on display `:1`
  (1280x800). Only processes holding the agent's X cookie can connect.
- `/home/agent` and `/workspace` live on a separate data disk mounted at
  `/data`, so rebuilding the system disk keeps files and browser logins.
- Services are supervised by OpenRC: `kivora-display`, `kivora-wm`,
  `kivora-browser`, `kivora-vnc` and `kivora-web`.
- Chromium runs with `--disable-gpu-shader-disk-cache`: on Alpine, the GPU
  process's sandbox refuses the system call that writes this cache, and a new
  profile then crashes the browser.
- Chromium DevTools (9222), VNC (5900) and the web gateway (6080) listen on
  localhost only. Over SSH, the `agent` account may only forward to 6080. Its
  authorized keys are in `/etc/ssh/authorized_keys/agent`, on the data disk,
  and it cannot change them: use `kivora add-laptop`.

### Its network

The VM is attached to no network of the server. Its Ethernet frames end in
`kivora-net`, a TCP/IP stack in a process of the container, which only opens
connections to the Internet on its behalf. Nothing inside the VM can lift this.

- **Refused:** 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16,
  172.16.0.0/12, 192.0.0.0/24, 192.0.2.0/24, 192.168.0.0/16, 198.18.0.0/15,
  198.51.100.0/24, 203.0.113.0/24, 224.0.0.0/4, 240.0.0.0/4, and the addresses of the
  container and of the server. A name that resolves to one of them is refused when
  the VM connects.
- **IPv4 and TCP only.** No other UDP than DNS on its gateway (10.86.0.1), no ping.
  The browser falls back from QUIC to TCP.
- **The server's addresses.** From its container, `kivora-net` cannot see a public
  address of the server. The `host-addresses` service reads them: it runs once at each
  `up`, with the server's network and no other right, and has no access to the VM's
  data. After a change of address, run `docker compose up -d`.
- **Exceptions.** `kivora allow add|remove|list` manages the private services the VM
  may reach: an address, optionally a CIDR and a port, over TCP only. They are kept in
  the volume and apply to the next connection.

`kivora check-isolation` tests this from the VM against the server's addresses, its
router and the address of Docker's gateway, each first tried from the container.

### The container

- QEMU and `kivora-net` run as the `kivora-vm` account, without capabilities, QEMU
  under its seccomp sandbox.
- The admin commands run as `kivora`. The SSH keys, the backups and their passphrase
  are in a directory of that account: should the VM ever get out of QEMU, they are out
  of its reach.
- The entrypoint stays root, with SETUID and SETGID only, to start each part under its
  account. The root filesystem is read-only, and `no-new-privileges` is set.
- The VM's host key is created in the volume before the first boot: the admin commands
  and laptops know it from the start, and it survives `kivora recover`.

## Maintenance

All commands run on the server and keep the data disk (`/home/agent`,
`/workspace`: files, browser profile, logins) unless stated otherwise.

```sh
kivora backup                 # encrypted backup of the data disk
kivora restore --list         # backups, most recent first
kivora restore --yes [FILE]   # reset the data (backs up the current data first)
kivora recover --yes          # rebuild a broken system around the data
```

To update:

```sh
git pull
docker compose up -d --build
```

When the new image brings something new for the VM, the container upgrades Alpine's
packages, installs it in the VM, and restarts the VM on a new kernel. It does not back
up first: run `kivora backup` before. Between two updates,
`kivora ssh 'doas apk update && doas apk upgrade --available'` upgrades the packages,
and `kivora ssh 'doas reboot'` restarts the VM if that brought a new kernel.

Backups are point-in-time copies taken while the VM runs: QEMU starts the copy with
the guest filesystem frozen for a moment, then copies in the background. They are
encrypted with GPG (AES-256), using a passphrase created by the first backup.

Backups and the passphrase live in the volume, so `docker compose down -v` deletes
them, and so does losing the server. **Copy them elsewhere, the passphrase above all**:
without it, backups cannot be restored.

```sh
docker compose cp kivoradesk:/var/lib/kivoradesk/admin/backups ./backups
docker compose cp kivoradesk:/var/lib/kivoradesk/admin/backup.passphrase ./backup.passphrase
```

To bring them to a new server, once the [install](#on-the-server) is done there:

```sh
docker compose exec -T kivoradesk kivora import-passphrase < backup.passphrase
docker compose exec -T kivoradesk kivora import-backup < backups/kivoradesk-data-YYYYMMDDTHHMMSSZ.qcow2.gpg
docker compose exec kivoradesk kivora restore --yes
```

Laptop keys are on the data disk and come back with it. The VM of the new server has
its own host key: on the laptop, run `ssh-keygen -R kivoradesk`, then add the
`known_hosts` line that `kivora add-laptop` prints.

## Credentials

```sh
kivora secret set github-password https://github.com   # prompts for the value
kivora secret set --field text github-login https://github.com
kivora secret list                                     # names, sites, field kinds
kivora secret delete github-password
```

A credential goes into password fields only. With `--field text`, it also goes
into login, email and one-time code fields, which sites may display: keep that
for values that are not really secret, such as a login name. Credentials stored
by an older version count as password credentials: store a login name again with
`--field text`.

Values are stored in clear in the VM, on the data disk, readable only by the
`kivora-vault` account, and included in the encrypted backups of the data disk.
The MCP server asks that account for one value at a time, for the page it is
about to fill. This keeps them from the model and the browser's pages, not from
code running as the desktop account, which could ask for them too.

## Live view from the laptop

The web gateway (`kivora-web`) streams the desktop with noVNC and decides who
owns the input: **agent**, **paused** or **human**. Viewer input only reaches
the desktop while the human owns it; switching away releases any held key or
button.

On the laptop, once [installed](#on-your-laptop), keep the tunnel open:

```sh
ssh -N kivoradesk
```

On the server, get a sign-in URL and open it in the laptop's browser:

```sh
kivora web-url            # works once, for 5 minutes
kivora web-url --rotate   # also signs out every browser
```

The input owner survives a restart of the gateway or of the VM: if the agent did
not own the desktop, it stays paused until you hand control back.

The link gives the browser a session of 30 days, in a strict same-site cookie, and
is of no use after that. On the VM, sessions are only stored as hashes. The gateway
also rejects non-loopback `Host` headers and cross-origin control requests.

`kivora.localhost` keeps the cookie away from the other services of the laptop on
`localhost`. If your browser does not resolve it (Safari may not), add
`127.0.0.1 kivora.localhost` to `/etc/hosts`.

## Scenario suite

`kivora-scenarios` measures how reliably complete workflows run (goal: at least
90 % over 30 scenarios). It drives the real MCP server over SSH with
scripted tool sequences, so it measures the computer, not a model's
reasoning:

| Area | Scenarios |
|---|---|
| Web | search, read a long page and a truncated one, follow links, back/forward/reload, tabs, slow page, redirects, navigate with the keyboard |
| Forms and typing | fill and submit a form, Unicode through CDP and through the keyboard, site search box |
| Files from the web | PDF, binary download, download with the browser's cookies, download then process in the shell |
| Desktop | screenshot and scroll, windows and focus |
| Shell and files | pipeline, files round trip, timeout, worker isolation |
| Human control | input lock, takeover request |
| Persistence and faults | login kept across a browser restart, credentials, Chromium crash, Internet loss |

Run it on the server, in this directory, with the server set up as a
[laptop](#on-your-laptop) and the live view tunnel open:

```sh
ssh -N kivoradesk &
go build -o bin/kivora-scenarios ./cmd/kivora-scenarios
bin/kivora-scenarios -mcp "ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp" \
  -token "$(docker compose exec -T kivoradesk kivora ssh 'doas -u kivora-web /usr/local/bin/kivora-web session')" \
  -admin "docker compose exec -T kivoradesk kivora" -runs 10
```

Result on 2026-11-05 (Alpine 3.24, Chromium 155, 2 vCPU, 4 GiB, in Docker): **300/300
runs passed (100 %)**, 10 runs per scenario. Without Internet, navigation
fails with an explicit error, at once or after its 45-second timeout.

## Benchmark

`kivora-bench` reboots the VM and measures boot time, disk use, memory by
component, idle CPU, the latency of the main tools and memory with real sites
open. `-compare` shows the change between two runs, to judge an optimization.
Results and method: [docs/benchmark.md](docs/benchmark.md).

```sh
go build -o bin/kivora-bench ./cmd/kivora-bench
bin/kivora-bench -label before -out before.json   # then change something
bin/kivora-bench -label after -out after.json
bin/kivora-bench -compare before.json after.json
```

## Tests

Unit tests run anywhere: `go test ./...`.

Integration tests run against the VM, on the server, with the server set up as a
[laptop](#on-your-laptop) and the live view tunnel open (`ssh -N kivoradesk`):

```sh
KIVORA=(docker compose exec -T kivoradesk kivora)
KIVORA_WEB_URL=http://localhost:6080 \
KIVORA_WEB_TOKEN=$("${KIVORA[@]}" ssh 'doas -u kivora-web /usr/local/bin/kivora-web session') \
KIVORA_XDOTOOL="${KIVORA[*]} ssh doas -u agent env DISPLAY=:1 xdotool" \
KIVORA_MCP_COMMAND="ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp" \
go test -tags integration -p 1 ./cmd/...   # one at a time: they share the VM
```

The MCP tests drive the real server over SSH: browsing, forms, accented
typing, PDF and download, shell and files, the input lock and the takeover
request. `KIVORA_TYPE_RUNS=100` sets the number of accented typing runs.
`KIVORA_ADMIN="${KIVORA[*]}"` adds the tests that use the server's admin commands:
fault injection (Chromium is killed, the VM loses its Internet route, and the
tools must recover or fail explicitly) and a credential that must never
reach the model.

### Input accuracy probe

```sh
CGO_ENABLED=0 go build -o bin/kivora-probe ./cmd/kivora-probe
docker compose exec -T kivoradesk kivora ssh 'cat > /tmp/kivora-probe && chmod 755 /tmp/kivora-probe' < bin/kivora-probe
docker compose exec -T kivoradesk kivora ssh 'doas -u agent env DISPLAY=:1 /tmp/kivora-probe -n 1000'
```

Run `kivora-probe -h` for options (`-seed` for reproducible runs, `-json`).

## Contributing

The project follows git flow (`main`, `develop`, `feature/*`, `release/*`,
`hotfix/*`) and [Conventional Commits](https://www.conventionalcommits.org/).
To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Claude Code

KivoraDesk does not modify or redistribute Claude Code. Installing and using
Claude Code is governed by Anthropic's terms; each user must authenticate with
their own authorized account.

## License

Apache-2.0 — see [LICENSE](LICENSE).

Kivo, the marmot in `docs/assets/`, is the project mascot and is covered by
the same license.
