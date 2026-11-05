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

- **The machine around the VM.** The VM cannot reach the server, the local network,
  VPNs, containers or other VMs, unless you allow it with `kivora allow`.
- **The desktop from the agent's commands.** Commands from `shell_run` and `files_*`
  run under an account with no access to the display, the browser or the VM's
  services, and with no network except the sites you allow.
- **The human's control.** The agent cannot take the desktop back from the human, or
  act behind the input lock.
- **Credentials from the model.** Values stored with `kivora secret` are not
  shown to the agent, and are removed from the text of tool results.
- **The audit log from the agent.** It can add entries, not read or change them.

A way around any of these is a vulnerability.

## What it does not protect

- **What the browser sends.** The browser can reach the whole Internet: an agent
  misled by a web page can send to a site what it read elsewhere. Do not leave
  it signed in to accounts it does not need.
- **What sites display.** Screenshots cannot be cleaned, and a site that shows a
  stored value shows it on screen.
- **The port you publish.** The VM's SSH is only published on the server's
  `127.0.0.1` by default. Docker's published ports bypass ufw and firewalld: with
  `KIVORA_BIND=0.0.0.0` on a public server, it is open to the Internet, with keys only.
  Prefer a LAN or VPN address.
- **Your laptop's access to the server.** With a `ProxyJump` through your own account
  on the server, an agent on the laptop inherits that access.
- **Decisions made by the model.** KivoraDesk does not judge what the agent chooses
  to do. It limits where that can reach.
- **Data at rest on the server.** Credentials are stored in clear on the VM's data
  disk, readable by the vault account. Anyone with root on the server, or access to
  its Docker, can read them.
