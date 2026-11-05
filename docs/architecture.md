# Architecture and security model

How KivoraDesk is built, and where each boundary sits. For what is and is not protected, see [SECURITY.md](../SECURITY.md).

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
  at a time, across MCP sessions too, and report when control moved during the
  action. Observing tools are refused while the human uses the desktop. Only the
  human gives control back; the agent can ask for help, which pauses it and shows
  the reason in the live view.
- **Privilege separation.** The MCP server runs as `agent`, which owns the
  display and the browser. `shell_run` and `files_*` run as `worker`, which has
  no access to the display, any service of the VM, including SSH on its own
  address, or the control API (local firewall by UID, `doas` limited to the
  helper). Each account only sees its own processes. VNC is reserved to the
  gateway and DevTools to `agent`. Code the agent runs cannot drive the desktop
  behind the lock.
- **Untrusted content.** Page text, titles, addresses, file names and content,
  window names, command output and what a server says in an error are wrapped as
  untrusted data, in a tag with a random ID that the content cannot close. Titles
  and addresses are cut when they are too long. Screenshots cannot be wrapped. Tools
  declare MCP annotations (read-only, destructive).
- **Browser tools.** Their scripts run in an isolated world: a page cannot change
  what they read, pretend to be the visible tab or move the numbers of its
  elements. `browser_navigate` refuses loopback addresses and `browser_download` the
  addresses of the VM itself. They do not reach inside iframes and shadow DOM:
  `browser_read_page` says how many it skipped, and `secrets_fill` cannot fill a field
  there. `pdftotext` and `file` are installed to read what `browser_save_pdf` and
  `browser_download` save.
- **Shell network.** Shell commands have no network of their own, not even DNS.
  Their only way out is a proxy reaching, over HTTPS, the sites you allow with
  `kivora shell-allow`, and none at first. It logs what it opens and refuses
  in `/var/log/kivora-proxy.log`, which is rotated. `browser_download` is how files
  get in. The browser is not limited: the agent can still send to a site what it reads
  elsewhere.
- **Shell limits.** Each `shell_run` has its own PID, IPC and mount namespaces, which
  needs unprivileged user namespaces, with no capability and no way to create more.
  Nothing it starts outlives the call, even detached: neither processes, shared memory,
  nor `/tmp`, `/var/tmp` and `/dev/shm`, which are private, small and empty at each call.
  Keep files in `/workspace`. 512 processes, 3 GiB per process and 8 GiB per file at
  most, and low priority. There is no quota on `/workspace` as a whole, only a reserve
  of the data disk for the audit log, the credentials and the browser. A command is the
  first killed when memory runs out, unless it changes that itself. Cancelling a call
  stops its command.
- **Typing.** `browser_type` inserts any text exactly through CDP.
  `computer_type` types ASCII with the keyboard and pastes other characters
  through the clipboard, checking control between chunks.
- **Audit.** Every call is logged as a JSON line with a trace ID in
  `/var/lib/kivora-audit/audit-YYYY-MM-DD.jsonl` on the data disk, kept 90 days,
  under an account of its own: the agent's side can add entries, not read, change
  or delete them. A call that could not be logged says so in its result.
  Common forms of passwords, tokens and keys are masked in the logged arguments, as a
  safety net. A `secrets_fill` entry names the site that received the value.
- **Timeouts and recovery.** Every call has a maximum duration, waiting for another
  one included. A page held by a dialog or a loop gives an error within seconds. After a
  Chromium crash, browser tools wait for the service to restart it; without
  Internet they fail at once with the browser's network error.
- **Credentials.** The human stores credentials with `kivora secret`,
  each bound to the sites where it may be used and to a kind of field. They live
  under a separate account that the browser cannot read. The agent sees names,
  sites and field kinds only; `secrets_fill` checks the page origin and the field
  and fills it in one step, and keeps it masked on screen. Stored values and their
  common encodings are removed from the text of every tool result, as a safety net: a
  site can still show a value in a form this does not recognize. Screenshots cannot be
  cleaned: a site that displays a value it received shows it on screen. Once logged in,
  the agent can still read what the site itself displays.
- **Browser lockdown.** Chromium policies block local files, browser pages,
  the developer tools, extensions, file dialogs, downloads from the browser and
  its password manager, for the agent and for the human during a takeover. They are
  in `/etc/chromium/policies/managed/kivora.json` in the VM, and Chromium's flags in
  `KIVORA_BROWSER_FLAGS`, in `/etc/conf.d/kivoradesk`. An update, a restore or a
  recovery installs these files again: keep your changes, and apply them again with
  `kivora ssh`. Tools refuse to read anything but web pages. `browser_download` and
  `browser_save_pdf` are the way to get files.
- **Files to the laptop.** Copy them with `scp kivoradesk:/workspace/<path> .`
  Transfers run as `worker`, in `/workspace`: a link left there cannot lead to the
  agent's private files. `scp -O`, the old protocol, is refused.

## Inside the VM

- The data disk is mounted without setuid programs or device files, and
  `/etc/sysctl.d/kivoradesk.conf` hardens the kernel for local accounts.
- The desktop runs as the unprivileged `agent` user on display `:1`
  (1280x800). Only processes holding the agent's X cookie can connect.
- `/home/agent`, `/workspace`, the credentials, the audit log, the laptops' keys and
  the live view's sessions and input owner live on a separate data disk mounted at
  `/data`, so rebuilding the system disk keeps them. A reserve of this disk stays out
  of the worker's reach, so that they keep working when `/workspace` is full.
- Services are supervised by OpenRC: `kivora-display`, `kivora-wm`,
  `kivora-browser`, `kivora-vnc`, `kivora-proxy` and `kivora-web`.
- Chromium runs with `--disable-gpu-shader-disk-cache`: on Alpine, the GPU
  process's sandbox refuses the system call that writes this cache, and a new
  profile then crashes the browser.
- Chromium DevTools (9222), VNC (5900) and the web gateway (680) listen on
  localhost only. The gateway's port is privileged, so no other account of the VM can
  take it. The `agent` account's SSH keys only start the MCP server and file transfers,
  and may only forward to the gateway. They are in `/etc/ssh/authorized_keys/agent`, on
  the data disk, and it cannot change them: use `kivora add-laptop` and `kivora laptops`.

## The VM's network

The VM is attached to no network of the server. Its Ethernet frames end in
`kivora-net`, a TCP/IP stack in a process of the container, which only opens
connections to the Internet on its behalf. Nothing inside the VM can lift this.

- **Refused:** 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 161.26.0.0/16,
  168.63.129.16, 169.254.0.0/16, 172.16.0.0/12, 192.0.0.0/24, 192.0.2.0/24,
  192.88.99.0/24, 192.168.0.0/16, 198.18.0.0/15, 198.51.100.0/24, 203.0.113.0/24,
  224.0.0.0/4, 240.0.0.0/4, and the addresses of the container, of the server and of
  its router. Its DNS leaves them out of its answers, and a name that resolves to one
  of them is refused when the VM connects.
- **IPv4 and TCP only.** No other UDP than DNS on its gateway (10.86.0.1). No ping to
  the Internet: only the gateway answers one. The browser falls back from QUIC to TCP.
- **The server's addresses.** From its container, `kivora-net` cannot see a public
  address of the server. The `host-addresses` service reads those on its interfaces and
  its default router: it runs once at each `up`, with the server's network and no other
  right, and has no access to the VM's data. After a change of address, run `docker
  compose up -d`. If this list cannot be read, the VM's connections are refused. A
  public address on none of the interfaces, behind NAT as on most cloud servers, must
  be listed in `KIVORA_PUBLIC_ADDRESSES`.
- **Exceptions.** `kivora allow add|remove|list` manages the private services the VM
  may reach: an address, optionally a CIDR and a port, over TCP only. They are kept in
  the volume and apply to the next connection.

`kivora check-isolation` tests this from the VM against the server's addresses, its
router and the address of Docker's gateway, each first tried from the container.

## The container

- QEMU and `kivora-net` run as the `kivora-vm` account, without capabilities, QEMU
  under its seccomp sandbox. Memory and processes are limited: what the VM causes must
  not exhaust the server.
- The admin commands run as `kivora`. The SSH keys, the backups and their passphrase
  are in a directory of that account: should the VM ever get out of QEMU, they are out
  of its reach.
- These accounts have the UIDs 64101 and 64102. Docker shares UIDs with the server: an
  account of the server with the same one could reach their files, so these are out of
  the range of human accounts.
- The entrypoint stays root, with SETUID and SETGID only, to start each part under its
  account. The root filesystem is read-only, and `no-new-privileges` is set.
- The VM's host key is created in the volume before the first boot: the admin commands
  and laptops know it from the start, and it survives `kivora recover`.
