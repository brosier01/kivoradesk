# Security

KivoraDesk gives an AI agent a computer, so a flaw here can matter. Thanks for
reporting one.

## Reporting a vulnerability

Use [GitHub's private reporting](https://github.com/brosier01/kivoradesk/security/advisories/new)
(**Security** tab, **Report a vulnerability**). Please do not open a public issue
for a vulnerability.

Include what you did, what happened and which version you ran
(`docker compose exec kivoradesk kivora doctor` prints it). This is a personal project: expect a first
answer within a week, on a best-effort basis.

Only the latest release gets security fixes.

## What KivoraDesk protects

- **The machine around the VM.** The VM cannot connect to the server's network
  addresses, its router, the local network, VPNs, containers, other VMs or cloud
  metadata services, unless you allow it with `kivora allow`. For the addresses it has
  no way to know, see below.
- **The desktop from the agent's commands.** Commands from `shell_run` and `files_*`
  run under an account with no access to the display, the browser or the VM's
  services, and with no network except the sites you allow.
- **The human's control.** The agent cannot take the desktop back from the human, or
  act behind the input lock.
- **The VM from a laptop's key.** A key added with `kivora add-laptop` gives the MCP
  server, the tunnel to the live view and file transfers as the worker. It opens no
  shell: an agent allowed to run `ssh` on the laptop gets no more than its tools.
- **Credentials from the model.** Values stored with `kivora secret` are not given to
  the agent: it can only have one filled into a matching field of its site.
- **The audit log from the agent.** It can add entries, not read or change them.

A way around any of these is a vulnerability.

## What it does not protect

- **What the browser sends.** The browser can reach the whole Internet: an agent
  misled by a web page can send to a site what it read elsewhere. Do not leave
  it signed in to accounts it does not need.
- **What sites display.** The text of tool results is cleaned of stored values and of
  their common encodings, as a safety net: a site can still show a value in a form this
  does not recognize. Screenshots cannot be cleaned, and a site that shows a stored
  value shows it on screen.
- **What you publish to the Internet.** A service of the server reached through a
  router's port forwarding, or on a public address that is on none of the server's
  interfaces, as on most cloud servers, is on the Internet: the VM can reach it as
  anyone can. List these addresses in `KIVORA_PUBLIC_ADDRESSES`. A public address that
  changes is only known at the next `docker compose up`.
- **The port you publish.** The VM's SSH is only published on the server's
  `127.0.0.1` by default. Docker's published ports bypass ufw and firewalld: with
  `KIVORA_BIND=0.0.0.0` on a public server, it is open to the Internet, with keys only.
  Prefer a LAN or VPN address.
- **Your laptop.** What runs under your account there can use its SSH key and read the
  live view's session from the browser. With a `ProxyJump` through your own account on
  the server, an agent on the laptop inherits that access. The session's cookie also
  goes to the other ports of `kivora.localhost`: keep that name for KivoraDesk.
- **Decisions made by the model.** KivoraDesk does not judge what the agent chooses
  to do. It limits where that can reach.
- **Updates.** Nothing updates itself. Chromium reads hostile pages, and its package
  only moves with an update of KivoraDesk or your own `apk upgrade`; `kivora doctor`
  warns when the VM is behind. The container's QEMU and OpenSSH only move when its
  image is rebuilt: see [Maintenance](README.md#maintenance).
- **The VM's own resources.** There is no quota on `/workspace`, only a reserve of the
  data disk for the audit log, the credentials and the browser. A command can use the
  VM's memory while it runs, and can ask not to be the first killed. This can stop the
  agent's work, not the server.
- **The VM's kernel.** Shell commands share it with the browser and the credentials.
  They have no capability and cannot create namespaces, which removes much of what it
  exposes, not all: a kernel flaw reachable from an unprivileged process gives root in
  the VM.
- **Data at rest on the server.** Credentials are stored in clear on the VM's data
  disk, readable by the vault account. Anyone with root on the server, or access to
  its Docker, can read them. Docker shares UIDs with the server: the container's
  accounts use 64101 and 64102 so that no human account does. Do not give these IDs to
  an account of the server.
