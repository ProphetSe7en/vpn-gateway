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

```bash
docker pull ghcr.io/prophetse7en/vpn-gateway:latest
```

### Run

Use this image as a drop-in replacement for `ghcr.io/hotio/base:alpinevpn`. All hotio configuration (WireGuard, port forwarding, DNS, etc.) works exactly the same.

```bash
docker run -d \
  --name vpn-gateway \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv4.conf.all.src_valid_mark=1 \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  -v /path/to/config:/config \
  -e VPN_ENABLED=true \
  -e VPN_CONF=wg0 \
  -p 6050:6050 \
  -e VPN_EXPOSE_PORTS_ON_LAN=6050/tcp \
  -e PRIVNET=192.168.86.0/24 \
  ghcr.io/prophetse7en/vpn-gateway:latest
```

On first start, a default `traffic.conf` is created in `/config/` with all options documented.

## Web UI

The web UI is available on port **6050**. To enable it:

1. Map port 6050 in your container config (`-p 6050:6050`)
2. Add `6050/tcp` to `VPN_EXPOSE_PORTS_ON_LAN` so hotio's firewall allows LAN access
3. Open `http://<server-ip>:6050` in your browser

The UI provides:
- Current status (active rates, limited/unlimited badge)
- Default rate and burst buffer settings
- Schedule rule editor with day filters
- Effective schedule summary showing what rates apply when

Changes saved via the UI are written to both `/config/.traffic-ui.json` (UI model) and `/config/traffic.conf` (bash config). The config watcher picks up changes within 10 seconds.

You can also edit `traffic.conf` manually — the UI reads whichever file is newer.

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

## Credits

Built on [hotio/base:alpinevpn](https://hotio.dev/containers/base/) by [hotio](https://hotio.dev). All VPN functionality (WireGuard, firewall, DNS leak protection) is provided by the hotio base image. vpn-gateway only adds the bandwidth management layer.

## License

MIT
