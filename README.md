# KivoraDesk

A self-hosted, persistent and isolated Linux computer that an AI agent can use
like a person does: look at the screen, move the mouse, type, browse the web,
run commands and keep files.

KivoraDesk runs an Alpine Linux VM on your own server (QEMU/KVM + libvirt).
Claude Code stays on your laptop and drives the VM through an MCP server reached
over SSH. You can watch the desktop live and take over at any time.

> **Status:** early development. Phases 0 to 4 are done: Claude Code on a
> laptop drives the VM through the MCP server (web research, forms, PDFs,
> shell, files, credentials) while the human watches and can take over, with
> backups, recovery, a 30-scenario reliability suite at 100 % and a
> published [benchmark](docs/benchmark.md).

## How it works

```text
Laptop                              Homelab server (Linux, KVM)
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
  no access to the display, Chromium DevTools, VNC, the gateway or the control
  API (local firewall by UID, `doas` limited to the helper). Code the agent
  runs cannot drive the desktop behind the lock.
- **Untrusted content.** Page text, file content and command output are wrapped
  as untrusted data. Tools declare MCP annotations (read-only, destructive).
- **Typing.** `browser_type` inserts any text exactly through CDP.
  `computer_type` types ASCII with the keyboard and pastes other characters
  through the clipboard, checking control between chunks.
- **Audit.** Every call is logged as a JSON line with a trace ID in
  `/home/agent/.local/state/kivora/audit/audit-YYYY-MM-DD.jsonl`, kept 90 days.
  Passwords, tokens and keys are masked in the logged arguments.
- **Timeouts and recovery.** Every call has a maximum duration. After a
  Chromium crash, browser tools wait for the service to restart it; without
  Internet they fail at once with the browser's network error.
- **Credentials.** The human stores credentials with `scripts/vm/secret.sh`,
  each bound to the sites where it may be used. The agent sees names and sites
  only; `secrets_fill` checks the page origin and fills the field in one step,
  and masks it on screen. Password fields and filled fields are never returned
  by `browser_read_page`. Once logged in, the agent can still read what the
  site itself displays.
- **Files to the laptop.** Copy them with `scp kivoradesk:/workspace/<path> .`

## Use with Claude Code

On the laptop, with the SSH configuration from `scripts/vm/ssh-config.sh`:

```sh
claude mcp add --scope user kivoradesk -- ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp
```

Then ask Claude Code, for example, to research a topic on the web and save the
sources as PDFs in `/workspace`, while you watch it in the live view.

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
| 5 | Installation guide and first release | Planned |

## Repository layout

```text
cmd/kivora-bench/   resource and latency benchmark, A/B comparison
cmd/kivora-mcp/     MCP server (and the worker helper it runs as worker)
cmd/kivora-probe/   input accuracy probe, runs inside the VM
cmd/kivora-scenarios/ workflow reliability suite over MCP
cmd/kivora-web/     web gateway: live view, takeover, input control state
internal/browser/   structured browser tools over CDP
internal/control/   input owner state (agent, paused, human) and agent API
internal/desktop/   native mouse, keyboard, screenshots and windows
internal/worker/    shell commands and file operations as the worker
internal/workspace/ symlink-safe writes into /workspace
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
scripts/vm/doctor.sh                    # diagnose the VM, its services and the MCP server
scripts/vm/allow.sh add 192.168.1.20:8080 staging   # let the VM reach one private service
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
LAN router. `allow.sh add|remove|list` manages exceptions (an address,
optionally a CIDR and a TCP port), kept in `~/.local/share/kivoradesk/allowlist`
on the host and applied to the running VM at once.

Use `virsh shutdown --mode agent kivoradesk` for a clean shutdown: the Alpine
virt image has no ACPI power button.

## Maintenance

All scripts run on the server and keep the data disk (`/home/agent`,
`/workspace`: files, browser profile, logins) unless stated otherwise.

```sh
scripts/vm/backup.sh                          # encrypted backup of the data disk
scripts/vm/backup.sh --install-cron "0 3 * * *"   # daily at 3:00 (crontab)
scripts/vm/restore.sh --list                  # backups, most recent first
scripts/vm/restore.sh --yes [FILE]            # reset the data (backs up the current data first)
scripts/vm/update.sh [--reboot]               # backup, Alpine upgrade, redeploy, diagnostics
scripts/vm/recover.sh --yes                   # rebuild a broken system around the data
```

Backups are point-in-time copies taken while the VM runs: libvirt starts the
copy with the guest filesystem frozen for a moment, then copies in the
background. They are encrypted with GPG (AES-256) using the passphrase in
`~/.local/share/kivoradesk/backup.passphrase` and stored in
`~/.local/share/kivoradesk/backups` (`KIVORA_BACKUP_DIR`), seven kept
(`KIVORA_BACKUP_KEEP`). **Keep a copy of the passphrase file elsewhere**:
without it, backups cannot be restored.

## Credentials

```sh
scripts/vm/secret.sh set github-password https://github.com   # prompts for the value
scripts/vm/secret.sh list                                     # names and sites
scripts/vm/secret.sh delete github-password
```

Values are stored in the VM, readable only by the desktop account, and
included in the encrypted backups of the data disk.

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

## Scenario suite

`kivora-scenarios` measures how reliably complete workflows run (PRD target:
at least 90 % over 30 scenarios). It drives the real MCP server over SSH with
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

```sh
go build -o bin/kivora-scenarios ./cmd/kivora-scenarios
bin/kivora-scenarios -mcp "ssh -i $KEY -T agent@192.168.122.50 /usr/local/bin/kivora-mcp" \
  -token "$(scripts/vm/ssh.sh 'doas cat /var/lib/kivora-web/token')" \
  -scripts "$PWD/scripts/vm" -runs 10
```

Result on 2026-11-01 (Alpine 3.24, Chromium 155, 2 vCPU, 4 GiB): **300/300
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

Integration tests run against the VM, with the gateway tunnel open
(`ssh -N -L 6080:127.0.0.1:6080 agent@192.168.122.50` from the server, using
a key authorized with `authorize-key.sh`):

```sh
KEY=~/.local/share/kivoradesk/ssh/id_ed25519
KIVORA_WEB_URL=http://localhost:6080 \
KIVORA_WEB_TOKEN=$(scripts/vm/ssh.sh 'doas cat /var/lib/kivora-web/token') \
KIVORA_XDOTOOL="$PWD/scripts/vm/ssh.sh doas -u agent env DISPLAY=:1 xdotool" \
KIVORA_MCP_COMMAND="ssh -i $KEY -T agent@192.168.122.50 /usr/local/bin/kivora-mcp" \
go test -tags integration -p 1 ./cmd/...   # one at a time: they share the VM
```

The MCP tests drive the real server over SSH: browsing, forms, accented
typing, PDF and download, shell and files, the input lock and the takeover
request. `KIVORA_TYPE_RUNS=100` sets the number of accented typing runs.
`KIVORA_HOST_SCRIPTS=1` adds the tests that use the server's admin scripts:
fault injection (Chromium is killed, the VM loses its Internet route, and the
tools must recover or fail explicitly) and a credential that must never
reach the model.

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
