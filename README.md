# Towamori Agent

The host agent for [Towamori](https://towamori.com) server monitoring.
It reads CPU, memory, disk and network metrics on a Linux server and sends them to Towamori over HTTPS.

It is small on purpose: Go standard library only, no third-party dependencies, and few enough lines
that you can read all of it before you install it.

## What it will never do

You are installing a long-running program on your server, so here is exactly what it can and cannot do.

- **Outbound only.** It never listens on a port.
- **No remote control.** It never accepts commands. It cannot run a shell, open a terminal, or read
  or write files on request. The only thing the platform can change is how often it reports
  (5–300 seconds).
- **No root needed.** The systemd unit runs it as a throwaway unprivileged user with a read-only view
  of the system.
- **Numbers, not data.** It sends counters and a few facts about the machine. It does not read
  process names, command lines, environment variables, file contents, logs or user accounts.
- **No plain-text tokens.** It refuses to send its token over `http://` unless the target is
  `localhost`.
- **See it before you trust it.** `towamori-agent --once` prints exactly what would be sent, without
  touching the network.

## Look before you install

```sh
towamori-agent --once
```

This takes two readings one second apart and prints the `hello` and `report` request bodies as JSON.
That output is everything the agent ever sends.

## Install

On a Linux server with systemd (amd64 or arm64), with the token from your Towamori dashboard:

```sh
curl -fsSL https://raw.githubusercontent.com/towamori/agent/main/packaging/install.sh \
  | sudo TOWAMORI_TOKEN=<your token> sh
```

The script downloads the release binary from GitHub, checks it against the published SHA-256
checksums, installs it to `/usr/local/bin`, writes your token to `/etc/towamori/agent.env`
(readable only by root) and starts the `towamori-agent` systemd service.
Read [`packaging/install.sh`](packaging/install.sh) first if you prefer; it is short.

Manual install: download `towamori-agent_linux_<arch>.tar.gz` from the
[releases page](https://github.com/towamori/agent/releases), put the binary in `/usr/local/bin`,
copy [`packaging/systemd/towamori-agent.service`](packaging/systemd/towamori-agent.service) to
`/etc/systemd/system/`, create `/etc/towamori/agent.env` with your token, then run
`systemctl enable --now towamori-agent`.

No sudo, or prefer Docker? Run the agent in a container that still measures the host
(build the image from [`packaging/docker/Dockerfile`](packaging/docker/Dockerfile)):

```sh
docker run -d --name towamori-agent --restart unless-stopped \
  --pid host --network host --uts host \
  -v /:/host:ro,rslave -e TOWAMORI_HOST_ROOT=/host \
  --env-file /etc/towamori/agent.env \
  towamori-agent
```

`TOWAMORI_HOST_ROOT` tells the agent where the host's `/` is mounted; it then reads the host's
mount table instead of the container's.

Check it is running:

```sh
systemctl status towamori-agent
journalctl -u towamori-agent -f
```

Uninstall:

```sh
sudo systemctl disable --now towamori-agent
sudo rm /etc/systemd/system/towamori-agent.service /usr/local/bin/towamori-agent
sudo rm -r /etc/towamori
sudo systemctl daemon-reload
```

## Configuration

Settings are read from flags, then environment variables, then `/etc/towamori/agent.env`
(`KEY=VALUE` lines).

| Flag | Environment | Default | |
|---|---|---|---|
| `-token` | `TOWAMORI_TOKEN` | — | Agent token from the dashboard. Required. |
| `-endpoint` | `TOWAMORI_ENDPOINT` | `https://ingest.towamori.com` | Where to send metrics. |
| `-interval` | `TOWAMORI_INTERVAL` | `10s` | How often to collect, 5s–300s. Your plan may set a different interval. |
| `-env-file` | | `/etc/towamori/agent.env` | Ignored if it does not exist. |
| `-host-root` | `TOWAMORI_HOST_ROOT` | `/` | Where the host's `/` is mounted when the agent runs in a container. |
| `-once` | | | Print one reading and exit. No token or network needed. |
| `-print` | | | Also print every request body to stdout. |
| `-version` | | | Print the version and exit. |

Keep the token in the env file (as the install script does) rather than passing `-token` on the
command line: command-line arguments are visible to every user on the machine through `ps`.

The agent honours the standard `HTTPS_PROXY` and `NO_PROXY` environment variables.

## What it collects

| Group | Fields | Read from |
|---|---|---|
| Host (once, on start) | hostname, OS name and version, kernel, CPU model, core count, memory size, boot time, a salted hash of `/etc/machine-id` | `/proc/sys/kernel/*`, `/etc/os-release`, `/proc/cpuinfo`, `/proc/meminfo`, `/proc/stat`, `/etc/machine-id` |
| CPU | usage, user, system, iowait, steal (%) | `/proc/stat` |
| Load | 1, 5, 15 minute load average | `/proc/loadavg` |
| Memory | total, available, used, swap total, swap used | `/proc/meminfo` |
| Disks | per real filesystem: mount point, type, total, used, available, inodes | `/proc/self/mounts`, `statfs(2)` |
| Disk I/O | read and write bytes/s across physical disks | `/proc/diskstats` |
| Network | receive and transmit bytes/s per interface, and totals (VPN tunnels are listed but not double-counted) | `/proc/net/dev` |
| Other | uptime, number of processes (a count, not a list) | `/proc/uptime`, `/proc` |

The full wire format is in [`docs/protocol.md`](docs/protocol.md).

Metrics are collected on Linux only. On other systems the agent runs (useful for development) but
sends host information only.

## Build from source

Go 1.22 or newer:

```sh
make test     # go vet + tests with the race detector
make build    # bin/towamori-agent for this machine
make dist     # dist/: static linux/amd64 and linux/arm64 archives + checksums.txt
```

## Develop against a local server

`towamori-devserver` is a small stand-in for the platform. It checks every request against the
protocol and prints one line per sample.

```sh
go run ./cmd/towamori-devserver -token dev
go run ./cmd/towamori-agent -env-file "" -endpoint http://127.0.0.1:8787 -token dev
```

## Contributing, security, license

- Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
- Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).
- The code is licensed under the [Apache License 2.0](LICENSE).
- "Towamori" and the Towamori logo are not covered by that license; see [TRADEMARKS.md](TRADEMARKS.md).
