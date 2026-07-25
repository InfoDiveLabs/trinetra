# Deployment guide

Step-by-step for getting `serverwatch` running on a Linux/systemd home server and
verifying it works. For the architecture and design rationale, see
[DESIGN.md](DESIGN.md).

## 0. Prerequisites

- A Linux host with **systemd** (Ubuntu/Debian/Raspberry Pi OS, etc.).
- Root (the service runs as root so it can read every container/process, SMART
  data, and start at boot without a login).
- A Telegram bot token from **@BotFather** (`/newbot` → copy the token).
- Optional: Docker (auto-discovered), `smartmontools` for SMART health
  (`apt install smartmontools`), and a [healthchecks.io](https://healthchecks.io)
  check URL for the real-time dead-man switch.

## 1. Get the binary

**Prebuilt** — download the right arch from the
[Releases](https://github.com/Suraj-Tiwari/server-monitor/releases) page:

| Host | Asset |
|------|-------|
| x86-64 server / NUC | `serverwatch-linux-amd64` |
| Raspberry Pi 3/4/5 (64-bit OS) | `serverwatch-linux-arm64` |
| Older 32-bit Pi / ARMv7 | `serverwatch-linux-arm` |

```bash
curl -fsSL -o /tmp/serverwatch \
  https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/serverwatch-linux-arm64
chmod +x /tmp/serverwatch
```

Verify the checksum against `checksums.txt` on the release:

```bash
sha256sum /tmp/serverwatch     # compare to the matching line in checksums.txt
```

**Or build from source** (Go 1.22+):

```bash
git clone git@github.com:Suraj-Tiwari/server-monitor.git
cd server-monitor
make linux                     # produces dist/serverwatch-linux-amd64 and -arm64
scp dist/serverwatch-linux-arm64 myserver:/tmp/serverwatch
```

## 2. Install as a systemd service

```bash
sudo /tmp/serverwatch install
```

This copies the binary to `/usr/local/bin/serverwatch`, writes
`/etc/systemd/system/serverwatch.service` (`Restart=always`,
`WantedBy=multi-user.target`), seeds `/etc/serverwatch/config.json` (mode 0600) if
absent, and runs `systemctl daemon-reload` + `enable --now`. The daemon is now
running and will start on every boot.

## 3. Connect Telegram

```bash
sudo serverwatch telegram set-token <token-from-BotFather>
```

Then **send your bot any message** (e.g. `hi`). The daemon captures your chat id
from that first message automatically — no need to set it by hand. Confirm:

```bash
# in Telegram:
/stats      # current CPU/mem/swap/load/temp + disks + internet
/help       # full command list
```

Commands: `/stats` `/status` `/disk` `/net` `/docker` `/services` `/history [days]`
`/down` `/help`.

## 4. Verify discovery and permissions

```bash
sudo serverwatch doctor
```

Prints what the host exposes: docker access method (socket / group / sudo /
unavailable), whether `smartctl` works, thermal-zone count, and the total number
of discovered targets. Anything it can't reach (no docker, no SMART) is reported
as unavailable and simply isn't monitored — it never crashes the daemon.

List every discovered target and its on/off + threshold:

```bash
sudo serverwatch monitor list
```

## 5. Tune what you care about (all optional)

Everything is CLI-managed; changes hot-reload via SIGHUP (no restart needed,
except `sample_interval` — see note):

```bash
sudo serverwatch config set thresholds.disk_pct 85     # alert when any fs >= 85%
sudo serverwatch config set thresholds.temp_c 75
sudo serverwatch monitor disable docker:some-noisy-container
sudo serverwatch monitor threshold disk:/boot 70       # per-target override
sudo serverwatch schedule daily 09:00                  # daily digest at 9am
sudo serverwatch schedule weekly mon@09:00             # weekly rollup Monday 9am
sudo serverwatch quiet-hours 23-8                       # suppress non-critical pings overnight
sudo serverwatch config get                             # show the effective config
```

> `sample_interval` changes take effect on the next daemon restart:
> `sudo systemctl restart serverwatch`.

## 6. Real-time "server is down NOW" alert (optional)

The daemon reconstructs downtime from its own heartbeat and reports it on the
next boot ("back online, was down 02:14→06:47"). For an *instant* alert while the
box is actually offline, point it at a [healthchecks.io](https://healthchecks.io)
dead-man switch — when the box (or its internet) drops, healthchecks pings your
Telegram directly:

```bash
sudo serverwatch healthchecks set https://hc-ping.com/<your-uuid>
```

## 7. Managing the service

```bash
systemctl status serverwatch            # is it running?
journalctl -u serverwatch -f            # live logs
serverwatch status                      # current snapshot (also /var/lib/serverwatch/status.json)
sudo serverwatch config get             # effective config
sudo systemctl restart serverwatch      # after a sample_interval change or upgrade
```

## 8. Upgrading

Download/build a newer binary and re-run install (it overwrites the binary and
reloads the unit; your config and 30-day history are preserved):

```bash
sudo /tmp/serverwatch install
```

## 9. Uninstalling

```bash
sudo serverwatch uninstall            # stop + remove the unit (keeps config + history)
sudo serverwatch uninstall --purge    # also delete /etc/serverwatch and /var/lib/serverwatch
```

## On-disk layout

```
/usr/local/bin/serverwatch                  the binary
/etc/serverwatch/config.json                config + secrets (0600)
/etc/systemd/system/serverwatch.service     the unit
/var/lib/serverwatch/
  status.json        current snapshot
  heartbeat          last-alive timestamp (drives downtime reconstruction)
  samples/*.jsonl    per-day metric samples (30-day retention)
  downtime.jsonl     power_down + net_down events (30-day)
  baseline.json      rolling per-metric mean/variance
  alerts.json        active-alert state (dedup + recovery)
```

## Troubleshooting

- **No Telegram replies:** confirm the token (`serverwatch config get`), and that
  you messaged the bot at least once so it captured the chat id. Check
  `journalctl -u serverwatch -f` for API errors.
- **Docker not monitored:** run `serverwatch doctor`. If it says docker
  unavailable, either add the service user to the `docker` group or ensure
  passwordless `sudo docker` works — the daemon falls back to `sudo` automatically
  when the socket isn't directly reachable (it runs as root by default, so the
  socket is normally reachable).
- **SMART unavailable:** `apt install smartmontools`. Some disks/USB bridges don't
  expose SMART; those are simply skipped.
- **Alerts too noisy/quiet:** adjust `thresholds.*`, `baseline_sigma`, or disable
  specific targets with `monitor disable <target>`.
