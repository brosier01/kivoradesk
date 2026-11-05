# Administration

Every `kivora COMMAND` runs on the server, in the clone's directory, as `docker compose exec kivoradesk kivora COMMAND`. `kivora` alone lists the commands.

## Everyday commands

```sh
kivora ssh                                     # open a shell as the admin user
kivora doctor                                  # diagnose the VM, its services and the MCP server
kivora check-isolation                         # VM reaches the Internet, not private networks
kivora allow add 192.168.1.20:8080 staging     # let the VM reach one private service
kivora shell-allow add pypi.org                # let shell commands reach one site
kivora laptops list                            # the laptops' keys, with their fingerprints
kivora laptops remove FINGERPRINT              # stop trusting a laptop
docker compose down                            # shut the VM down, keep everything
docker compose down -v                         # delete the VM and all its data, backups included
```

## Laptops

When the laptop is another machine, the VM's SSH port must be reachable from it. By default it
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

The key gives the MCP server, the live view's tunnel and file transfers, and opens no
shell on the VM. For diagnosis, use `kivora ssh` on the server. `kivora laptops` lists
the keys and removes one.

## Starting and stopping

The VM stops with the container and restarts with Docker (`restart: unless-stopped`):
it comes back when Docker starts with the server, unless you stopped it yourself.
Stopping the container shuts the VM down cleanly, through its guest agent when SSH
does not answer.

## Settings

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
| `KIVORA_BACKUP_KEEP` | 7 | backups kept, at least 1 |
| `KIVORA_PUBLIC_ADDRESSES` | | public addresses of the server that are on none of its interfaces, for the VM to refuse them |
| `KIVORA_VERSION` | from the git tags | version the image is built with, shown by `kivora doctor` |

**Limits.** Without UDP, video calls in the VM's browser may fail (untested).

## Maintenance

All commands run on the server and keep the data disk (`/home/agent`,
`/workspace`: files, browser profile, logins) unless stated otherwise.

```sh
kivora backup                 # encrypted backup of the data disk
kivora restore --list         # backups, most recent first
kivora restore --yes [FILE]   # reset the data (backs up the current data first)
kivora restore --yes --no-backup [FILE]   # the same, when that backup cannot be made
kivora recover --yes          # rebuild a broken system around the data
kivora repair-data --yes      # repair the data disk's filesystem
kivora resume                 # continue a VM stopped by a full volume
```

The install never formats a data disk that holds something. If that disk cannot be
mounted, or a disk cannot be opened, the container stays up without a working desktop,
so that `kivora doctor`, `restore`, `recover` and `repair-data` still answer. A restore
installs what the image brings in the VM again, and brings back the laptops' keys of
that backup: check them with `kivora laptops list`.

To update:

```sh
git pull
docker compose up -d --build
```

When the new image brings something new for the VM, the container upgrades Alpine's
packages, installs it in the VM, and restarts the VM on a new kernel. It does not back
up first: run `kivora backup` before. Nothing updates by itself. Between two updates,
`kivora ssh 'doas apk update && doas apk upgrade --available'` upgrades the packages,
and `kivora ssh 'doas reboot'` restarts the VM if that brought a new kernel. `kivora
doctor` warns when the VM's packages are behind.

Docker's cache keeps QEMU and OpenSSH as they were when the image was built. For their
security updates, rebuild it:

```sh
docker compose build --pull --no-cache
docker compose up -d
```

Backups are point-in-time copies taken while the VM runs: QEMU starts the copy with
the guest filesystem frozen for a moment, then copies in the background. They are
encrypted with GPG (AES-256), using a passphrase created by the first backup.

A backup checks first that the volume has room for it. Old backups are removed by
`KIVORA_BACKUP_KEEP`, never during a restore.

Backups and the passphrase live in the volume, so `docker compose down -v` deletes
them, and so does losing the server. **Copy them elsewhere, the passphrase above all**:
without it, backups cannot be restored. Git ignores these two names:

```sh
docker compose cp kivoradesk:/var/lib/kivoradesk/admin/backups ./backups
docker compose cp kivoradesk:/var/lib/kivoradesk/admin/backup.passphrase ./backup.passphrase
```

To bring them to a new server, once the [install](../README.md#1-on-the-server) is done there:

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
for values that are not really secret, such as a login name. A site is a scheme, a
host and an optional port, with no wildcard. A value is stored as typed, or refused if
it is not valid UTF-8 or longer than 64 KiB.

Values are stored in clear in the VM, on the data disk, readable only by the
`kivora-vault` account, and included in the encrypted backups of the data disk.
The MCP server asks that account for one value at a time, for the page it is
about to fill, and to clean the text of its results. This keeps them from the model
and the browser's pages, not from code running as the desktop account, which could
ask for them too. Neither the worker's commands nor a laptop's key run as that account.

## Live view from the laptop

The web gateway (`kivora-web`) streams the desktop with noVNC and decides who
owns the input: **agent**, **paused** or **human**. Viewer input only reaches
the desktop while the human owns it; switching away releases any held key or
button.

On the laptop, once [installed](../README.md#2-on-your-laptop), keep the tunnel open:

```sh
ssh -N kivoradesk
```

On the server, get a sign-in URL and open it in the laptop's browser:

```sh
kivora web-url            # works once, for 5 minutes
kivora web-url --rotate   # also signs out every browser and cuts their connections
kivora web-url --port N   # the link for a tunnel on another local port
```

The input owner survives a restart of the gateway or of the VM and `kivora recover`:
if the agent did not own the desktop, it stays paused until you hand control back.

The link gives the browser a session of 30 days, in a strict same-site cookie, and
is of no use after that. When the session ends, the page says so and how to sign in
again. On the VM, sessions are only stored as hashes. The gateway also rejects
non-loopback `Host` headers and cross-origin control requests.

`kivora.localhost` keeps the cookie away from the other services of the laptop on
`localhost`. It still goes to other ports of that name: keep it for KivoraDesk. If your
browser does not resolve it (Safari may not), add `127.0.0.1 kivora.localhost` to
`/etc/hosts`.
