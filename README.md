# KivoraDesk

<p align="center">
  <img src="docs/assets/kivo.png" width="256" alt="Kivo, the KivoraDesk marmot, browsing the web from a desk in its burrow">
</p>

A self-hosted, persistent and isolated Linux computer that an AI agent can use
like a person does: look at the screen, move the mouse, type, browse the web,
run commands and keep files.

KivoraDesk runs an Alpine Linux VM on your server, in a Docker container. Claude
Code stays on your laptop and drives the VM through an MCP server reached over SSH.
You watch the desktop live and can take over at any time.

```text
Laptop                              Server (Linux x86_64, KVM)
┌──────────────────────┐            ┌──────────────────────────────────────┐
│ Claude Code          │── SSH ────▶│ VM (Alpine Linux)                    │
│  └ MCP client        │            │  ├ MCP server                        │
│ Browser: live view   │── tunnel ─▶│  ├ desktop + Chromium                │
└──────────────────────┘            │  └ /workspace (durable files)        │
                                    └──────────────────────────────────────┘
```

- **Isolated.** The VM reaches the Internet, not your network. Shell commands run
  under a separate account with no network except the sites you allow.
- **Persistent.** Files, browser sessions and logins survive reboots.
- **Under your control.** One input owner at a time: a human takeover pauses the
  agent. Every tool call is logged where the agent cannot change it.
- **Credentials stay hidden.** The agent can have a stored password filled in, never
  read it.

> **Status:** early, first public release. Tested on Ubuntu 24.04 with Docker 29.1.

## Requirements

- **Server:** Linux x86_64 with KVM, Docker Engine 25+ with Compose v2, 2 vCPU,
  4 GiB of memory for the VM, about 32 GiB of disk.
- **Laptop:** OpenSSH and an MCP client such as Claude Code. It can be the server
  itself.

## Quick start

### 1. On the server

```sh
git clone https://github.com/brosier01/kivoradesk.git
cd kivoradesk
docker compose up -d --wait
docker compose exec kivoradesk kivora doctor   # every line should start with ok
```

The first start takes a few minutes. If Docker says `/dev/kvm` does not exist,
turn on virtualization in the firmware, or pick a server that exposes KVM.

Below, `kivora COMMAND` stands for `docker compose exec kivoradesk kivora COMMAND`.

### 2. On your laptop

Authorize the laptop's public key on the server:

```sh
kivora add-laptop "$(cat ~/.ssh/id_ed25519.pub)"   # run where the key is, or paste it
```

Run on the laptop the snippet it prints, then register the MCP server:

```sh
claude mcp add --scope user kivoradesk -- ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp
```

The VM's SSH is only published on the server's `127.0.0.1:2222`. From another
machine, set `KIVORA_BIND` to a LAN or VPN address of the server in `.env` and pass
it to `add-laptop`, or use `ProxyJump` through your account on the server: see
[administration](docs/administration.md).

### 3. Watch it

```sh
ssh -N kivoradesk     # on the laptop: keeps the tunnel open
kivora web-url        # on the server: a sign-in link, valid once for 5 minutes
```

Open the link in the laptop's browser. Then ask Claude Code, for example, to research
a topic and save the sources as PDFs in `/workspace`.

## Everyday commands

```sh
kivora doctor                   # check everything
kivora secret set NAME URL      # store a credential for a site
kivora shell-allow add pypi.org # let shell commands reach a site
kivora backup                   # encrypted backup of the data
git pull && docker compose up -d --build   # update
```

## Documentation

- [Administration](docs/administration.md): settings, laptops, backups, updates,
  credentials, live view.
- [Architecture and security model](docs/architecture.md): MCP tools, accounts,
  network, container.
- [Security policy](SECURITY.md): what is protected, what is not, how to report a flaw.
- [Development](docs/development.md): layout, tests, scenario suite, benchmark.
- [Benchmark](docs/benchmark.md) and [changelog](CHANGELOG.md).

## Contributing

The project follows git flow (`main`, `develop`, `feature/*`, `release/*`,
`hotfix/*`) and [Conventional Commits](https://www.conventionalcommits.org/).

KivoraDesk does not modify or redistribute Claude Code. Using Claude Code is governed
by Anthropic's terms, with each user's own account.

## License

Copyright 2026 Bruce Rosier. Apache-2.0, see [LICENSE](LICENSE). Kivo, the mascot in
`docs/assets/`, is covered by the same license.
