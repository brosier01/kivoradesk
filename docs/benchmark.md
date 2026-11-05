# KivoraDesk benchmark

Measured on 2026-11-01 with `kivora-bench`, on the libvirt install KivoraDesk had
then. Each change was measured on its own, after the previous ones (A/B), then all
together. The Docker install has another network and has not been measured on an idle
server yet: take the times and latencies below as an order of magnitude.

## Setup

- Host: AMD Ryzen 7 5825U, 28 GiB RAM, Ubuntu 24.04, KVM/libvirt, otherwise idle.
- VM: 2 vCPU, 4 GiB RAM, 16 GiB system disk, Alpine Linux 3.24.3,
  Chromium 155, kernel 6.18 (virt).
- Each run reboots the VM, waits one minute, then measures. Memory is
  `MemTotal - MemAvailable` (everything the kernel cannot reclaim, its own
  memory included) and, per component, the proportional set size (PSS: shared
  pages split between the processes that map them). Tool latency is the time
  of a call over SSH from the server, 20 runs each.

## Results

| | Before | After | Change |
|---|---:|---:|---:|
| Reboot to working MCP tools | 29.5 s | 13.3 s | −55 % |
| Kernel start to browser ready | 12.2 s | 5.1 s | −58 % |
| System disk used | 1021 MiB | 880 MiB | −14 % |
| Installed packages | 453 | 315 | −30 % |
| Memory used at idle | 598 MiB | 572 MiB | −4 % |
| Chromium at idle (PSS) | 455 MiB | 407 MiB | −10 % |
| Memory used with 3 sites open | 890 MiB | 871 MiB | −2 % |
| CPU at idle | 0.15 % | 0.15 % | = |

The three sites are English Wikipedia (Linux), github.com/torvalds/linux and
BBC News. The scenario suite (30 workflows, 10 runs each) stays at 300/300
after the changes.

## Changes, one at a time

| Step | Reboot to MCP | Browser ready (uptime) | Disk | Packages | Idle memory | Chromium PSS |
|---|---:|---:|---:|---:|---:|---:|
| Baseline (0.2.0) | 29.5 s | 12.2 s | 1021 MiB | 453 | 598 MiB | 455 MiB |
| Boot menu wait 10 s → 1 s | 19.2 s | 11.0 s | 1021 MiB | 453 | 606 MiB | 456 MiB |
| noVNC web client only, cloud-init removed after first boot | 18.2 s | 9.8 s | 889 MiB | 315 | 594 MiB | 464 MiB |
| Chromium `--disable-gpu` | 11.5 s | 5.4 s | 889 MiB | 315 | 581 MiB | 406 MiB |
| All, `/tmp` emptied at boot | 13.3 s | 5.1 s | 880 MiB | 315 | 572 MiB | 407 MiB |

- **Boot menu.** The Alpine cloud image waits 10 s at the limine menu on
  every boot.
- **noVNC.** The `novnc` package pulls websockify, Python and NumPy; the
  gateway only needs the web client, now extracted from the package.
- **cloud-init.** Needed on the first boot only (admin account, SSH keys,
  network configuration). It ran four services at every boot and kept Python
  installed.
- **GPU.** The VM has no GPU, so Chromium emulated one in a separate
  process, with no WebGL either way. Without it Chromium uses about 50 MiB
  less; Xvfb takes 16 MiB more, as part of the rendering moves there. The
  system-wide gain (6 to 10 MiB) is within the noise of a single run.

Reading the steps: each row is a single run. Idle memory varies by about
±10 MiB between runs and the reboot time by about ±2 s (the shutdown part),
so the last row can be slower than the one above it. The step to the GPU row also
holds about 4 s from a DHCP setting of the former network: the VM now has a fixed
address and no DHCP client.

## Tool latency

Unchanged by the optimizations (median of 20 calls, final configuration):

| Tool | p50 | p95 |
|---|---:|---:|
| `computer_get_state` | 6 ms | 7 ms |
| `computer_screenshot` | 50 ms | 52 ms |
| `computer_click` | 66 ms | 66 ms |
| `computer_key` | 45 ms | 46 ms |
| `computer_type`, 20 ASCII characters | 266 ms | 271 ms |
| `computer_type`, 20 characters with accents | 788 ms | 812 ms |
| `browser_navigate` example.com | 38 ms | 46 ms |
| `browser_read_page` | 2 ms | 5 ms |
| `shell_run true` | 4 ms | 5 ms |
| `files_write` + `files_read` | 6 ms | 7 ms |

Accented text is pasted through the clipboard, which stays in place 750 ms
after the target fetched it (Chromium drops a paste when the clipboard
changes too early).

## Tried and not kept

- Disabling the WebUI omnibox popup features (`WebUIOmniboxPopup`,
  `WebUIOmniboxFullPopup`): no change in processes or memory.
- Restarting the browser after 1 s instead of 3 s: shutdown 1 s faster,
  weaker protection against crash loops.
- Removing Mesa and LLVM (230 MiB): not possible, the Chromium package
  depends on them.

Not tried on purpose: turning off site isolation, limiting renderer
processes or discarding background tabs. They save memory with many tabs at
the cost of security or of what the agent sees.

## Earlier figures

The 0.1.0 notes gave 313 MiB used at idle: that was the "used" column of
`free`, which leaves out reclaimable kernel memory. On 2026-11-01, the same VM read
about 350 MiB there; the benchmark's stricter measure read about 570 MiB.

## Reproduce

On the server, with the VM created and provisioned, and Go 1.27.3:

```sh
go build -o bin/kivora-bench ./cmd/kivora-bench
bin/kivora-bench -label before -out before.json
# apply a change, docker compose up -d --build --wait
bin/kivora-bench -label after -out after.json
bin/kivora-bench -compare before.json after.json
```

`-reboot=false` skips the boot measurement; `-sites` and `-runs` change the
loaded pages and the number of calls per tool. Extra Chromium flags go in
`KIVORA_BROWSER_FLAGS` (`/etc/conf.d/kivoradesk` in the VM), which an update installs
again.
