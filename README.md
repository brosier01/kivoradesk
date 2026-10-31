# KivoraDesk

A persistent, isolated and self-hostable graphical Linux computer, designed to be driven by Claude Code.

KivoraDesk runs an Alpine Linux VM on a homelab host (QEMU/KVM + libvirt) with a
visible Chromium desktop, a durable `/workspace`, and a local MCP server exposing
reliable Computer Use primitives (screenshot, mouse, keyboard, windows). A human
can watch the desktop live, pause the agent and take over at any time.

> **Status:** early development (Phase 0 — feasibility). Nothing here is usable yet.

## Goals

- A real computer: a KVM guest separate from the host, with an X11 desktop and Chromium.
- Claude Code running inside the VM, with the MCP tools available.
- Reliable Computer Use, measured by a reproducible test suite.
- Persistence of files, browser profile and configuration across reboots.
- Human control: live view, instant pause, conflict-free takeover.
- Security: no VNC, CDP or MCP endpoint exposed publicly; least privilege.

## Non-goals (for now)

- Multi-tenant SaaS, orchestration of many VMs, billing.
- CAPTCHA solving or bypassing website restrictions.
- Autonomous root administration of the host.

## Claude Code

KivoraDesk does not modify or redistribute Claude Code. Installing and using
Claude Code is governed by Anthropic's terms; each user must authenticate with
their own authorized account.

## License

Apache-2.0 — see [LICENSE](LICENSE).
