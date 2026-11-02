# Security

KivoraDesk gives an AI agent a computer, so a flaw here can matter. Thanks for
reporting one.

## Reporting a vulnerability

Use [GitHub's private reporting](https://github.com/brosier01/kivoradesk/security/advisories/new)
(**Security** tab, **Report a vulnerability**). Please do not open a public issue
for a vulnerability.

Include what you did, what happened and which version you ran
(`scripts/vm/doctor.sh` prints it). This is a personal project: expect a first
answer within a week, on a best-effort basis.

Only the latest release gets security fixes.

## What KivoraDesk protects

- **The machine around the VM.** The VM cannot reach the server, the local network,
  VPNs or other VMs, unless you allow it with `scripts/vm/allow.sh`.
- **The desktop from the agent's commands.** Commands from `shell_run` and `files_*`
  run under an account with no access to the display, the browser or the VM's
  services, and with no network except the sites you allow.
- **The human's control.** The agent cannot take the desktop back from the human, or
  act behind the input lock.
- **Credentials from the model.** Values stored with `scripts/vm/secret.sh` are not
  shown to the agent, and are removed from the text of tool results.
- **The audit log from the agent.** It can add entries, not read or change them.

A way around any of these is a vulnerability.

## What it does not protect

- **What the browser sends.** The browser can reach the whole Internet: an agent
  misled by a web page can send to a site what it read elsewhere. Do not leave
  it signed in to accounts it does not need.
- **What sites display.** Screenshots cannot be cleaned, and a site that shows a
  stored value shows it on screen.
- **Your laptop's access to the server.** Without the jump account
  (`scripts/vm/jump-account.sh`), the SSH configuration uses your own account on the
  server, and an agent on the laptop inherits that access.
- **Decisions made by the model.** KivoraDesk does not judge what the agent chooses
  to do. It limits where that can reach.
- **Data at rest on the server.** Credentials are stored in clear on the VM's data
  disk, readable by the vault account. Anyone with root on the server can read them.
