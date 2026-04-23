# vpn-gateway

VPN gateway with nftables bandwidth management, scheduling, hot-reload, and per-interface rate limiting.

Built as a layer on top of [hotio/base:alpinevpn](https://hotio.dev/containers/base/) — adds bandwidth limiting via nftables `limit rate` rules, with time-based scheduling and automatic config hot-reload.

## Features

- **Web UI** — configure rules, rates, and schedule from a browser (port 6050)
- **Simple config** — set rates in MB/s, no conversion needed
- **Hot-reload** — edit `traffic.conf` and changes apply within 10 seconds, no restart required
- **Time-based scheduling** — different rates for different times of day and days of the week
- **Midnight carry-over** — schedule rules persist across midnight until the next rule takes over
- **VPN reconnect recovery** — watchdog detects when hotio rebuilds its nft table and re-applies rules
- **Upload + download** — independent rate limits for each direction
- **Burst control** — configurable burst buffer for smooth TCP throughput
- **Traffic stats** — real-time bandwidth graphs, 72-hour ring buffer, 365-day daily volumes, per-service breakdown
- **Per-service monitoring** — track bandwidth for qBittorrent (API), SABnzbd (API), and Dispatcharr (nftables counters for smooth 3s updates). Active Streams panel for Dispatcharr shows live channel/client info
- **Stats persistence** — all traffic data survives container restarts (saved every 5 min + on shutdown)

## How it works

nftables rate-limit rules are inserted into hotio's existing firewall chains. Traffic exceeding the configured rate is dropped (policing). TCP congestion control adapts to the limit — in testing, effective throughput was consistently ~97% of the configured rate.

All containers sharing the VPN gateway's network namespace (e.g., qBittorrent instances using `--net=container:vpn-gateway`) are affected by the same limits. The rate is aggregate, not per-container.

## Quick start

### Build locally

```bash
git clone https://github.com/ProphetSe7en/vpn-gateway.git
cd vpn-gateway
docker build -t vpn-gateway:latest .
```

### Pull from GHCR

> **⚠️ Use a pinned version tag, not `latest`.** This container manages your VPN and network routing — if an update introduces breaking changes, every container routed through it (qBittorrent, etc.) loses connectivity and won't recover until vpn-gateway is fixed or rolled back. Pin to a version and update manually when you're ready.

**Latest version: `v1.4.0`** — [all tags](https://github.com/prophetse7en/vpn-gateway/pkgs/container/vpn-gateway)

```bash
docker pull ghcr.io/prophetse7en/vpn-gateway:v1.4.0
```

### Run

Use this image as a drop-in replacement for `ghcr.io/hotio/base:alpinevpn`. All hotio configuration (WireGuard, port forwarding, DNS, etc.) works exactly the same.

```bash
docker run -d \
  --name vpn-gateway \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  -v /path/to/config:/config \
  -e VPN_ENABLED=true \
  -e VPN_CONF=wg0 \
  -p 6050:6050 \
  -e VPN_EXPOSE_PORTS_ON_LAN=6050/tcp \
  -e PRIVNET=192.168.86.0/24 \
  ghcr.io/prophetse7en/vpn-gateway:v1.4.0
```

On first start, a default `traffic.conf` is created in `/config/` with all options documented.

**First-run setup:** vpn-gateway redirects to `/setup` on first visit to create an admin account. See [Authentication](#authentication) below.

## Web UI

The web UI is available on port **6050**. To enable it:

1. Map port 6050 in your container config (`-p 6050:6050`)
2. Add `6050/tcp` to `VPN_EXPOSE_PORTS_ON_LAN` so hotio's firewall allows LAN access
3. Open `http://<server-ip>:6050` in your browser — first visit redirects to `/setup` to create an admin account

The UI has three tabs:
- **Traffic** — real-time throughput graph, per-service breakdown, Active Streams panel (Dispatcharr)
- **Volume** — historical bandwidth data (1h to all-time), per-service period summaries
- **Settings** — sidebar navigation with Bandwidth, Schedule, Service Monitoring, and Tools sections

Changes saved via the UI are written to both `/config/.traffic-ui.json` (UI model) and `/config/traffic.conf` (bash config). The config watcher picks up changes within 10 seconds.

You can also edit `traffic.conf` manually — the UI reads whichever file is newer.

## Authentication

As of `v1.4.0`, vpn-gateway requires a login before you can reach the web UI. The model mirrors Radarr/Sonarr's Security panel.

**Authentication** — how users log in:
- **Forms (login page)** *(default)* — standard username/password form + session cookie (30-day TTL).
- **Basic** — HTTP Basic Auth (browser popup). Use this only when a reverse proxy in front is already handling login.
- **None** — disables auth entirely. **Requires password confirmation to enable** because the blast radius is catastrophic: anyone who reaches the port is admin and can change rules, disable shaping, see credentials. Only safe on a host unreachable from other devices.

**Authentication Required** — who must log in:
- **Disabled for Trusted Networks** *(default)* — devices on the "trusted" CIDR list skip the login page. Convenient for LAN-only deployments.
- **Enabled (all traffic)** — every request needs credentials, even from your own LAN.

### First-run setup

1. Open `http://your-host:6050` — you'll be redirected to `/setup`
2. Create an admin username and password (min 10 characters, 2+ of upper/lower/digit/symbol)
3. You're logged in automatically and land in the main UI

Credentials are bcrypt-hashed (cost 12) and stored in `/config/auth.json`. Sessions persist across container restarts via `/config/sessions.json`.

### Trusted Networks

By default "trusted" means all private IPv4 + IPv6 ranges (RFC1918, link-local, ULA, loopback — Radarr-parity). **Anything in this list gets full admin access without a password** — that includes every other container on your Docker host and every device on your home WiFi.

To tighten: go to **Settings → Security** and list specific IPs/CIDRs:
- `192.168.86.0/24` — whole home VLAN
- `10.66.0.0/24` — WireGuard tunnel
- `192.168.86.22/32` — a single device

Loopback (`127.x`) is always trusted so Docker healthchecks work regardless of this list.

**Host-level lockdown:** set the `TRUSTED_NETWORKS` env var in your Unraid template or `docker-compose.yml` (same place as `VPN_ENABLED`, `VPN_CONF`, etc.) with the same comma-separated CIDR format. When set, the UI field is disabled with an amber banner — the trust boundary can only be changed by editing the template and restarting. Defends against UI-takeover attackers (session hijack, XSS, local-bypass peer).

### API Key

Every install gets an API key (visible in **Settings → Security**, rotatable). Send it on requests as:

```
X-Api-Key: <your-key>
```

or as a query parameter (legacy — leaks to access logs and browser history):

```
?apikey=<your-key>
```

Use this for scripts, Uptime Kuma, and any `/api/*` endpoint. API-key auth bypasses both the login requirement and CSRF protection.

**Exceptions — public endpoints (no auth needed):**
- `/api/health` — liveness probe for Docker HEALTHCHECK, Uptime Kuma, reverse-proxy health tests. Returns `{"ok":true}`.
- `/api/stats/widget` — formatted aggregate stats for Homepage. Same risk profile as `/api/health` (no secrets, no enumeration), kept public so existing Homepage installs keep working after the v1.4.0 upgrade without re-configuring.

### Homepage widget

The Homepage widget described below keeps working with no changes — `/api/stats/widget` is in the public-endpoint list above.

### Reverse-proxy deployment

Behind SWAG / Authelia / Traefik / Caddy that terminates TLS:
1. Set **Trusted Proxies** to the proxy's IP (or use `TRUSTED_PROXIES` env var to lock at host level — same format as `TRUSTED_NETWORKS`).
2. Ensure the proxy sends `X-Forwarded-For` and `X-Forwarded-Proto: https`.
3. Pick either **Forms** (vpn-gateway handles login) or **Basic** (reverse proxy handles login upstream).

vpn-gateway only trusts `X-Forwarded-*` headers when the direct peer IP matches a configured Trusted Proxy — prevents header spoofing from other containers on the same bridge network.

### Lost password recovery

No email reset flow — by design, `/config/auth.json` is authoritative. To recover:

1. Stop the container
2. Delete `/config/auth.json` (credentials only — your schedule, bandwidth config, and traffic stats all live in `/config/traffic.conf`, `/config/.traffic-ui.json`, and `/config/.traffic-stats.json`, untouched)
3. Start the container
4. Open the web UI — you'll be redirected to `/setup` again to create new credentials

This is safe on a machine where you have `/config` access. If someone ELSE can delete that file, they can also take over vpn-gateway — which is expected behavior for a local admin tool.

## Configuration

Edit `/config/traffic.conf` or use the web UI. Changes are detected automatically within 10 seconds.

### Default rates

```bash
# Values in MB/s. Set to 0 for unlimited.
DEFAULT_DOWN=75
DEFAULT_UP=75
```

### Schedule

Each rule says "from this time, use this rate". Rules stay active until the next rule takes over — even across midnight.

```bash
SCHEDULE_ENABLED=true

# Weekday nights: full speed
SCHEDULE_1_TIME="23:00"
SCHEDULE_1_DOWN=0
SCHEDULE_1_UP=0
SCHEDULE_1_DAYS="mon-thu"

# Weekday mornings: limited
SCHEDULE_2_TIME="06:00"
SCHEDULE_2_DOWN=75
SCHEDULE_2_UP=75
SCHEDULE_2_DAYS="mon-fri"

# Midday: full speed (everyone at work/school)
SCHEDULE_3_TIME="07:30"
SCHEDULE_3_DOWN=0
SCHEDULE_3_UP=0
SCHEDULE_3_DAYS="mon-fri"

# Afternoon/evening: limited
SCHEDULE_4_TIME="15:00"
SCHEDULE_4_DOWN=75
SCHEDULE_4_UP=75
SCHEDULE_4_DAYS="mon-fri"

# Weekends overnight: full speed
SCHEDULE_5_TIME="01:00"
SCHEDULE_5_DOWN=0
SCHEDULE_5_UP=0
SCHEDULE_5_DAYS="sat,sun"

# Weekends daytime: limited
SCHEDULE_6_TIME="11:00"
SCHEDULE_6_DOWN=75
SCHEDULE_6_UP=75
SCHEDULE_6_DAYS="sat,sun"
```

Day filters support ranges (`mon-fri`), lists (`mon,wed,fri`), single days (`tue`), or omit for every day.

### Advanced

```bash
# Burst buffer in milliseconds (default: 500)
# Higher = smoother throughput, Lower = stricter enforcement
BURST_MS=500

# Log rate changes to container log (default: true)
LOG_CHANGES=true
```

## Useful commands

```bash
# Show active rules and packet counters
docker exec vpn-gateway nft-apply status

# Remove all limits (unlimited)
docker exec vpn-gateway nft-apply clear

# Force re-read config now (instead of waiting 10s)
docker exec vpn-gateway nft-apply reload

# View rate changes and schedule triggers
docker logs vpn-gateway
```

## Config upgrades

When a new version adds config options, the service automatically adds missing options to your existing `traffic.conf` while preserving all your settings. A `CONFIG_VERSION` field tracks this — don't edit it manually.

## Architecture

```
traffic.conf ──→ svc-traffic (s6 service)
                   ├── nft-apply (insert/replace/delete nft rules)
                   ├── crond (schedule triggers + verify watchdog every 60s)
                   └── config watcher (md5sum poll every 10s → hot-reload)

svc-webui (s6 service, port 6050)
  ├── GET /api/status        — current rates + active rule
  ├── GET /api/config        — full config (JSON or parsed bash)
  ├── PUT /api/config        — save config (writes both JSON + bash)
  ├── GET /api/stats/stream  — SSE live traffic stats (3s intervals)
  ├── GET /api/stats/latest  — current stats snapshot
  ├── GET /api/stats/history — ring buffer history (1h/6h/24h/72h)
  ├── GET /api/stats/daily   — daily volume data (365 days)
  ├── POST /api/stats/reset  — clear all statistics
  └── static files           — Alpine.js SPA (embedded at build time)

Traffic measurement:
  wg0 rx_bytes - nft dropped bytes = actual VPN throughput
  (nft drops excess packets; wg0 counts them before drop)

nft rules are inserted into hotio's existing inet hotio table:
  output chain: upload limit before hotio's wg0 accept rule
  input chain:  download limit before hotio's wg0 ct state accept rule
```

## Traffic Monitoring

The Stats tab shows real-time and historical bandwidth data:

- **VPN-gateway total** — all traffic through the WireGuard tunnel (payload + TCP/IP headers + WireGuard encryption + protocol overhead)
- **Per-service totals** — application-level data for qBittorrent (API), SABnzbd (API), and Dispatcharr (nft byte counters). Each service shows individual download/upload rates and cumulative totals.

VPN total includes all tunnel traffic (data + protocol overhead + encryption). Per-service totals track application data only, so they will always be lower than the VPN total.

Stats are persisted to `/config/.traffic-stats.json` every 5 minutes and on graceful shutdown. Data includes a 72-hour ring buffer (3-second samples), 365 days of daily volumes, and per-service cumulative totals. The file is ~13 MB at maximum size and does not grow beyond that.

## Routing qBittorrent through vpn-gateway

Route one or more qBittorrent containers through the VPN gateway so all torrent traffic is encrypted, while the Web UI remains accessible on your LAN.

**[Step-by-step setup guide with screenshots](docs/qbittorrent-setup.md)** — covers TorGuard/WireGuard, port mapping, Docker Compose example, and multiple instances.

### Quick troubleshooting

**qBittorrent Web UI not accessible:**
- Check that the port appears in all three vpn-gateway locations (port mapping, `VPN_EXPOSE_PORTS_ON_LAN`, container port)
- Check that `WEBUI_PORTS` on qBit matches the container port on vpn-gateway
- Check vpn-gateway logs: `docker logs vpn-gateway`

**qBittorrent won't start:**
- Remove all port mappings from the qBit container — they conflict with container network mode
- Set `VPN_ENABLED=false` on qBit (or remove VPN variables entirely) — the gateway handles VPN

**Multiple instances conflict:**
- Each qBit instance must have a different `WEBUI_PORTS` value
- You cannot have two containers both listening on the same port on the same network stack

**Port forwarding not working:**
- Verify `VPN_PORT_REDIRECTS` format: `vpn_port@container_port/tcp`
- Verify qBittorrent's incoming connection port matches the container port (after `@`)
- Verify the port forward is active in your VPN provider's account

## Unraid

**Install via Community Apps:** Search for **vpn gateway** (without hyphen) in the Apps tab — click Install and configure your WireGuard settings.

**Or install manually:** Go to **Docker** → **Add Container**, set Repository to `ghcr.io/prophetse7en/vpn-gateway:v1.4.0`, and add the required paths, ports, and capabilities (see above).

The Web UI is available at `http://your-unraid-ip:6050`.

### Container Network Fix (Recommended)

When vpn-gateway restarts, Docker assigns it a new container ID. Containers using `container:vpn-gateway` network mode (like qBittorrent) keep the old reference and lose network connectivity. On Unraid, the only fix is to force-recreate each dependent container (edit → Apply without changes).

**[containernetwork-autofix](https://github.com/ProphetSe7en/containernetwork-autofix)** automates this — it detects when a network-parent container restarts and automatically recreates dependent containers. Install it alongside vpn-gateway to avoid manual intervention after restarts or updates.

> **Important:** Use the [ProphetSe7en/containernetwork-autofix](https://github.com/ProphetSe7en/containernetwork-autofix) fork — the original has parser bugs that cause it to miss containers with certain label formats.

**Updating:** Change the version tag in the Repository field to the new version, then click **Apply**. Do not use `latest` — see [Pull from GHCR](#pull-from-ghcr) for why.

## Homepage Widget

vpn-gateway has a built-in widget endpoint for [Homepage](https://gethomepage.dev/) dashboards. Add this to your `services.yaml`:

```yaml
- VPN Gateway:
    icon: https://raw.githubusercontent.com/prophetse7en/vpn-gateway/main/icon.png
    href: http://YOUR_IP:6050
    widget:
        type: customapi
        url: http://vpn-gateway:6050/api/stats/widget
        refreshInterval: 10000
        display: block
        mappings:
          - field: dlSpeed
            label: DL Speed
          - field: ulSpeed
            label: UL Speed
          - field: totalDl
            label: Total DL
          - field: totalUl
            label: Total UL
```

Replace `YOUR_IP` with your server IP, and `vpn-gateway` in the widget URL with the container hostname (or IP if Homepage is on a different Docker network).

### Available fields

| Field | Example | Description |
|-------|---------|-------------|
| `dlSpeed` | `45.2 MB/s` | Current download speed |
| `ulSpeed` | `12.8 MB/s` | Current upload speed |
| `totalDl` | `2.35 TB` | Total downloaded (since stats reset) |
| `totalUl` | `8.71 TB` | Total uploaded |
| `dailyDl` | `124.5 GB` | Downloaded in the last 24 hours |
| `dailyUl` | `48.3 GB` | Uploaded in the last 24 hours |

All values are pre-formatted — no `scale`, `suffix`, or `format` needed in Homepage. Pick the fields you want by adding or removing entries from `mappings`.

---

## Credits

Built on [hotio/base:alpinevpn](https://hotio.dev/containers/base/) by [hotio](https://hotio.dev). All VPN functionality (WireGuard, firewall, DNS leak protection) is provided by the hotio base image. vpn-gateway only adds the bandwidth management layer.

## Support

For questions, help, or bug reports:

- **Discord:** [`#prophetse7en-apps`](https://discordapp.com/channels/492590071455940612/1486391669384417300) on the [TRaSH Guides Discord](https://trash-guides.info/discord) (under Community Apps)
- **GitHub:** [prophetse7en/vpn-gateway/issues](https://github.com/prophetse7en/vpn-gateway/issues)

## License

MIT
