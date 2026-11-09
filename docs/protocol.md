# Towamori Agent Protocol — v1

This document is the complete contract between `towamori-agent` and the Towamori platform.
If something is not in here, the agent does not do it.

## Overview

- The agent makes **outbound HTTPS requests only**. It never listens on a port.
- There are exactly two endpoints: `hello` and `report`.
- The platform can change **one thing** on the agent: the reporting interval (5–300 seconds).
  There is no way for the platform to run commands, read files, or change anything else.

## Transport

| | |
|---|---|
| Base URL | `https://ingest.towamori.com` (configurable) |
| Method | `POST` |
| Body | JSON, gzip-compressed |
| Headers | `Authorization: Bearer <agent token>`<br>`Content-Type: application/json`<br>`Content-Encoding: gzip`<br>`User-Agent: towamori-agent/<version> (<os>/<arch>)`<br>`X-Towamori-Protocol: 1` |
| Timeout | 15 seconds per request |

The agent refuses to send its token over plain `http://` unless the host is a loopback address
(`localhost`, `127.0.0.0/8`, `::1`). This keeps local development possible without ever leaking a
token over the network.

The agent token is issued per host in the dashboard. It is opaque to the agent.

## Units

| Kind | Unit |
|---|---|
| Timestamps | Unix seconds, UTC (integer) |
| Sizes | bytes (integer) |
| Rates | bytes per second (float) |
| Percentages | 0–100 (float) |
| Durations | seconds (integer) |

Rates and CPU percentages are computed between two consecutive samples, so they are omitted on the
very first sample after the agent starts.

## `POST /v1/agent/hello`

Sent when the agent starts, and again if the platform asks for it (see *Responses*).

```json
{
  "protocol": 1,
  "agent": { "version": "0.1.0", "os": "linux", "arch": "amd64" },
  "host": {
    "hostname": "web-1",
    "machine_id": "3f1c9a0e5b7d2c4a8e6f1b3d5a7c9e0f",
    "os_name": "Ubuntu",
    "os_version": "24.04",
    "os_pretty": "Ubuntu 24.04.1 LTS",
    "kernel": "6.8.0-45-generic",
    "cpu_model": "AMD EPYC 7763 64-Core Processor",
    "cpu_cores": 2,
    "memory_total": 2061135872,
    "boot_time": 1794100000
  }
}
```

`machine_id` is **not** the raw `/etc/machine-id`. It is the first 32 hex characters of
`SHA-256("towamori:" + machine-id)`. The platform uses it only to notice when one token is being
used on two different machines. It is empty if `/etc/machine-id` does not exist.

Success response (`200`):

```json
{ "host_id": "h_9xk2", "interval_seconds": 10, "server_time": 1794200000 }
```

## `POST /v1/agent/report`

Sent after every collection. When the platform is unreachable the agent keeps up to 360 samples in
memory (one hour at the default interval) and sends them later, at most 60 per request, oldest first.

```json
{
  "protocol": 1,
  "host_id": "h_9xk2",
  "samples": [
    {
      "ts": 1794200010,
      "uptime": 100010,
      "procs": 142,
      "cpu": { "usage": 7.5, "user": 5.0, "system": 2.0, "iowait": 0.25, "steal": 0.25 },
      "load": { "l1": 0.12, "l5": 0.08, "l15": 0.05 },
      "mem": {
        "total": 2061135872, "available": 1288490188, "used": 772645684,
        "swap_total": 0, "swap_used": 0
      },
      "disks": [
        {
          "mount": "/", "fstype": "ext4",
          "total": 41943040000, "used": 12582912000, "avail": 27262976000,
          "inodes_total": 2621440, "inodes_used": 183500
        }
      ],
      "disk_io": { "read_bps": 0, "write_bps": 40960 },
      "net": {
        "rx_bps": 2048.5, "tx_bps": 1024.25,
        "rx_total": 987654321, "tx_total": 123456789,
        "ifaces": [
          { "name": "eth0", "rx_bps": 2048.5, "tx_bps": 1024.25 },
          { "name": "wg0", "rx_bps": 512, "tx_bps": 256, "tunnel": true }
        ]
      }
    }
  ]
}
```

Field notes:

- `cpu.*` — share of total CPU time across all cores since the previous sample. `usage` is
  everything except `idle` and `iowait`.
- `mem.used` is `total - available`, which matches what `free` calls "used" closely enough and does
  not count reclaimable page cache.
- `disks` — real block-device filesystems only (ext2/3/4, xfs, btrfs, zfs, f2fs, bcachefs, vfat,
  exfat, ntfs, and `fuseblk` for NTFS/exFAT disks mounted through FUSE). Bind mounts of the same
  device are reported once. `tmpfs`, `overlay`, `squashfs`, network filesystems and pseudo
  filesystems are skipped. A filesystem that does not answer `statfs` within 2 seconds is left out
  of that sample, and skipped until its earlier call returns, so one hung mount never stalls the
  agent.
- `disk_io` — summed over whole physical disks; partitions, `loop`, `ram`, `zram`, `dm-*` and `md*`
  are skipped so nothing is counted twice.
- `net` — `lo` and container/virtual bridges (`docker*`, `veth*`, `br-*`, `virbr*`, `cni*`,
  `flannel*`, `cali*`, `kube-*`, `vnet*`) are left out entirely. VPN and tunnel interfaces (`tun*`,
  `wg*`, `tailscale*`, `zt*`, `ipsec*`, `gre*`, `sit*`, `ip6tnl*`, `vti*`) appear in `ifaces` with
  `"tunnel": true` but are **not** added to `rx_bps`/`tx_bps`/`rx_total`/`tx_total`, because the
  same traffic also crosses a physical interface. `rx_total`/`tx_total` are the kernel counters since
  boot.

Success response (`202`):

```json
{ "accepted": 1 }
```

## Responses and agent behaviour

| Status | Meaning | Agent does |
|---|---|---|
| `200` / `202` | OK | Drops the sent samples from its buffer. If the body contains `interval_seconds`, switches to it (clamped to 5–300). If the body contains `"hello_required": true`, sends `hello` before the next report. |
| `401` / `403` | Token rejected | Logs it, keeps collecting into the buffer, retries `hello` every 5 minutes. |
| `413` | Batch too large | Halves the batch size for the next attempt. |
| `426` | Protocol no longer supported | Logs that an upgrade is needed and retries every hour. |
| `429` | Rate limited | Waits for `Retry-After` (seconds), or 60 seconds if missing. |
| other `4xx` | Bad request | Drops that batch (it will never succeed) and logs it. |
| `5xx`, network error | Temporary failure | Exponential backoff from 2 s to 5 min, with jitter. |

## Versioning

`protocol` is an integer that only changes for breaking changes. New optional fields can be added to
`v1` at any time; both sides must ignore fields they do not know.
