# Cercle

Cercle lets a small group of friends (2 to 20 people) exchange large files between their always-on NAS boxes. Everyone runs the same Docker container, browses the others' shared folders from a web page, ticks what they want, and the files download in the background.

Everything goes through a WireGuard network built into the app: only its UDP port is exposed to the Internet, and nothing needs to be configured by hand. An invitation code is all a friend needs to join.

## Requirements

- A NAS or Linux server with Docker (Synology, QNAP, Unraid, TrueNAS, Debian…), amd64, arm64 or armv7.
- Ideally a UDP port forwarded on your router. In every pair of members, at least one must be reachable.
- An address reachable from the Internet: a fixed IP, a DynDNS name or an IPv6 address.

## Installation

Start from [`deploy/compose.yml`](deploy/compose.yml) and adjust:

| Setting | Value |
| --- | --- |
| `user` | The NAS user and group owning the folders, as `uid:gid` (run `id` on the NAS) |
| `NODE_NAME` | The name other members will see |
| `WG_PUBLIC_ENDPOINTS` | Where others reach you: `alice.duckdns.org:51820`, `203.0.113.4:51820` or `[2001:db8::1]:51820`; comma-separated |
| `UI_PASSWORD` | Web interface password |
| Volumes | `/share`: what you share, read-only; mount one or more folders under it (`/mnt/nas/movies:/share/movies:ro`), then pick which ones to share in Settings. `/downloads`: what you receive. `/data`: keys and database, back it up |

- **Synology**: Container Manager, **Project**, **Create**, paste the adjusted compose file. If the DSM firewall is on, allow UDP 51820.
- **Unraid**: add a stack with the **Compose Manager** plugin; `user: "99:100"` fits default Unraid shares.
- **Linux**: `curl -O https://raw.githubusercontent.com/DB-CL/cercle/main/deploy/compose.yml`, adjust it, then `docker compose up -d`.

Then open `http://<nas-address>:8080/`.

On your router, forward **UDP 51820** to the NAS. It is the only port to open: keep the web interface (8080) on your local network. Behind a carrier-grade NAT (common on 4G/5G), use IPv6 instead, or open nothing: you can still exchange with reachable members.

## Configuration

Environment variables. Those shown in the Settings page (name, rates, windows, concurrency) can be changed live; the saved value then takes precedence over the environment.

| Variable | Default | Purpose |
| --- | --- | --- |
| `NODE_NAME` | hostname | Name shown to other members |
| `WG_LISTEN_PORT` | `51820` | WireGuard UDP port |
| `WG_PUBLIC_ENDPOINTS` | empty | Addresses where you can be reached, comma-separated |
| `TUNNEL_SUBNET` | `10.42.0.0/24` | Circle subnet, fixed when the circle is created |
| `UI_LISTEN` | `:8080` | Web interface address |
| `UI_PASSWORD` | empty | Login password; if empty, a random one is written to `ui.password` in the `/data` volume |
| `UPLOAD_LIMIT` | `0` | Total upload rate; K, M, G suffixes in powers of 1024 (`20M` = 20 MiB/s); `0` = unlimited |
| `MAX_UPLOADS` | `3` | Concurrent uploads; others wait their turn |
| `UPLOAD_WINDOW` | empty | Daily upload window, e.g. `01:00-07:00` |
| `DOWNLOAD_LIMIT` | `0` | Total download rate |
| `DOWNLOAD_CONCURRENCY` | `2` | Concurrent downloads (one at a time per member) |
| `DOWNLOAD_WINDOW` | empty | Daily download window |
| `SCAN_INTERVAL` | `1h` | How often the shared folder is indexed |
| `SHARE_EXCLUDE` | empty | Excluded patterns, e.g. `*.tmp,*.part` |
| `INVITE_TTL` | `72h` | Invitation code lifetime |
| `TZ` | `UTC` | Time zone for the windows, e.g. `Europe/Paris` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

Hidden files and folders, `@eaDir`, `#recycle` and `$RECYCLE.BIN` are never shared.

## Security

- Nothing but the WireGuard UDP port is exposed. The node-to-node API only listens inside the tunnel.
- Each node signs its own record (Ed25519): nobody can impersonate a member or join without an invitation.
- Members can only read the folders you ticked under `/share`, mounted read-only. Paths are confined on both the sending and the receiving side.
- The web interface always requires a password, with a signed session cookie and one login attempt per second at most.
- The container runs as non-root, with a read-only filesystem and no Linux capabilities.

## License

MIT, see [LICENSE](LICENSE). Third-party components and their licenses: [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
