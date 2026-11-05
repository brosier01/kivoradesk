# Development

The tools and tests here need Go 1.27.3, which the install does not.

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

## Tests

Unit tests run on Linux: `go test ./...`.

Integration tests run against the VM, on the server, with the server set up as a
[laptop](../README.md#2-on-your-laptop) and the live view tunnel open (`ssh -N kivoradesk`):

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
KIVORA=(docker compose exec -T kivoradesk kivora)
dir=$("${KIVORA[@]}" ssh 'mktemp -d')
"${KIVORA[@]}" ssh "cat > $dir/kivora-probe && chmod 755 $dir $dir/kivora-probe" < bin/kivora-probe
"${KIVORA[@]}" ssh "doas -u agent env DISPLAY=:1 $dir/kivora-probe -n 1000"
"${KIVORA[@]}" ssh "rm -rf $dir"
```

Run `kivora-probe -h` for options (`-seed` for reproducible runs, `-json`).

## Scenario suite

`kivora-scenarios` measures how reliably complete workflows run and checks that the
boundaries hold, over 41 scenarios (goal: at least 90 %). It drives the real MCP server over SSH with
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
| Pages | a hidden tab cannot pose as the visible one, a dialog or a busy page cannot hold the tools, a filled value stays hidden across sessions |
| Commands | a FIFO cannot hold the session, nothing outlives the call, no new namespace, their text stays out of the system log |
| Laptop | its key opens no shell, and works for MCP, file transfers and the tunnel |
| `-destructive` | backup retention, `kivora recover` during a takeover, a damaged data disk is repaired |

Without `-admin`, 30 of them run. `-admin` adds 8, and `-destructive` the last 3, once
each, since they restart the VM or damage its data disk: use it on a disposable
instance only. A scenario that cannot conclude says `skip`, and counts neither way.

Run it on the server, in this directory, with the server set up as a
[laptop](../README.md#2-on-your-laptop) and the live view tunnel open. Building it needs Go 1.27.3,
which the install does not:

```sh
ssh -N kivoradesk &
go build -o bin/kivora-scenarios ./cmd/kivora-scenarios
bin/kivora-scenarios -mcp "ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp" \
  -token "$(docker compose exec -T kivoradesk kivora ssh 'doas -u kivora-web /usr/local/bin/kivora-web session')" \
  -admin "docker compose exec -T kivoradesk kivora" -runs 10
```

Result on 2026-11-05 (Alpine 3.24, Chromium 156, 2 vCPU, 4 GiB, in Docker, from a
clean clone): **380/380 runs passed (100 %)**, 10 runs of each of the 38 scenarios
with `-admin`, and the 3 of `-destructive` passed. Without Internet, navigation
fails with an explicit error, at once or after its 45-second timeout.

## Benchmark

`kivora-bench` reboots the VM and measures boot time, disk use, memory by
component, idle CPU, the latency of the main tools and memory with real sites
open. `-compare` shows the change between two runs, to judge an optimization.
Results and method: [docs/benchmark.md](benchmark.md). Building it needs Go 1.27.3.

```sh
go build -o bin/kivora-bench ./cmd/kivora-bench
bin/kivora-bench -label before -out before.json   # then change something
bin/kivora-bench -label after -out after.json
bin/kivora-bench -compare before.json after.json
```

## Input accuracy

Measured on 2026-11-05 inside the VM with `kivora-probe` (Alpine 3.24, 2 vCPU,
4 GiB RAM, in Docker). Each action targets a random point; an oracle page loaded
over CDP checks the event type, button and exact screen coordinates.

| Action | Attempts | Success |
|---|---:|---:|
| Left click | 1000 | 100 % |
| Double click, right click, wheel | 100 each | 100 % |
| ASCII typing, Ctrl+A / Backspace | 100 / 25 | 100 % |
| Non-ASCII typing via xdotool (é, €, œ) | 100 | 21 % |

The MCP server avoids that limit: `computer_type` pastes non-ASCII text
through the clipboard and `browser_type` inserts text through CDP.

Resources: see the [benchmark](benchmark.md). In short, measured on
2026-11-05 in Docker on an idle server, with 2 vCPU and 4 GiB: 898 MiB of system
disk, about 585 MiB of memory used at idle and 880 MiB with three real sites open,
0.3 % of CPU at idle, and 13 s from a reboot to working MCP tools.
