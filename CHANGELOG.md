# Changelog

## v1.2.9

### Improvements
- **Unraid template: qBit port** — Template now includes a qBittorrent Web UI port field (7075) so users see it during install

## v1.2.8

### Improvements
- **Unlimited rule display** — Rules with rate 0 now show "Unlimited" in header instead of "↓0 ↑0 MB/s"
- **Default rate in header** — When no schedule rules are active but bandwidth limiting is on, the header now shows the default rate
- **Schedule hint** — Added "Set rate to 0 for unlimited" to schedule description
- **Unraid Community Apps** — Added Unraid installation instructions to README

## v1.2.7

### Improvements
- **TiB precision** — Volume totals now show 2 decimal places at TiB scale (e.g. "28.03 TiB" instead of "28.0 TiB"), so changes are visible without needing ~100 GiB of new traffic

## v1.2.6

### Improvements
- **Port monitoring docs** — UI and README now clarify that per-service stats only support qBittorrent (other apps still benefit from rate limiting and total VPN stats)

## v1.2.5

### Features
- **Dynamic stats grouping** — Volume charts auto-select day/week/month grouping based on available data (max ~60 points). 30d now shows 30 daily points instead of ~4 weekly
- **Unraid template** — Added `unraid-template.xml` with correct GHCR icon URL
- **Root icon** — `icon.png` in repo root for consistent Unraid/GitHub branding

### Bug fixes
- **Time parsing panic** — Malformed schedule time in manual config (e.g. missing colon) caused index out of bounds crash. Now safely skips invalid rules
- **Silent config errors** — Invalid numeric values in `traffic.conf` (e.g. `DEFAULT_DOWN=abc`) silently became 0. Now logs warnings
- **Channel close race** — Rapid SSE disconnections could panic on double channel close. Added existence check before closing
- **SSE reconnect race** — Manual `connectSSE()` calls during 3s reconnect timeout were silently dropped. Now cancels pending timer and connects immediately
- **parseInt radix** — Added explicit radix 10 to all `parseInt()` calls
- **Config copy error** — Silent failure if `/config` is read-only on first run. Now logs error message

## v1.2.4

### Features
- **Version footer** — App version displayed in UI footer (injected via build-time ldflags)

## v1.2.3

### Bug fixes
- **Stats volume accuracy** — Fixed inaccurate 6h/24h volume calculations when using downsampled data
- **Per-service graph** — Fixed graph showing only download in Both mode
- **Volume (All)** — Added All-time period option to volume charts
- **Traffic monitoring docs** — Added bandwidth monitoring documentation to README

## v1.2.2

### Features
- **App icon** — Favicon in browser tab, icon in UI header next to container name
- Icon files (`icon1.png`, `icon2.png`) available in repo root for Unraid template

## v1.2.1

### Features
- **Accurate traffic stats** — VPN throughput corrected by subtracting nft-dropped packets. Previously, wg0 counters included packets that the rate limiter subsequently dropped, inflating speeds by ~25%. Now shows actual throughput matching the configured rate limit (within ~3%)
- **Stats tab redesign** — Bar charts replaced with stacked area charts (separate Download/Upload). Smooth bezier curves, gradient fills, live updates every 3 seconds for short periods
- **Short period stats** — New 1h/6h/24h period options on Stats tab with 1-min/5-min/15-min buckets for granular volume tracking
- **VPN total toggle** — Stats volume charts show VPN total as background area alongside per-service stacked overlay. Total is always available, even without service ports configured
- **Summary table** — Period Summary replaces the old period totals grid. Shows per-service DL/UL/Total/Share%/Avg with VPN total row and overhead explanation
- **Rule change grace period** — 10-second cooldown after nft rule changes suppresses burst spikes in peak/avg/graph while keeping live speed display real-time
- **Per-service daily volumes** — Fixed inaccurate volume tracking that used `rate × interval` approximation. Now uses actual byte deltas from service API session counters
- **Traffic tooltip** — Redesigned with column layout (Total + per-service rows with DL/UL). Rate limit shown in tooltip instead of static graph line (which was misleading with changing limits)

### Changes
- Default UI port changed from 8090 to **6050** (configurable via `UI_PORT` env var)
- Stats tab service toggles now include VPN total button
- Data point dots hidden on short-period charts (shown only with ≤14 points)
- Improved text readability (schedule notes, header description)
- Per-service Total DL/UL correctly resets when using Reset Statistics

### Bug fixes
- **Peak exceeded configured limit** — wg0 `rx_bytes` counted packets dropped by nft shaper, inflating all rate calculations. Fixed by reading nft drop counters and subtracting from wg0 deltas
- **Reset didn't clear per-service totals** — Service cumulative bytes jumped back to qBit session total after reset. Fixed with session offset tracking
- **Reset didn't clear graph data** — Traffic graph and stats history retained pre-reset data. Now properly cleared on reset
- **Share% mixed data sources** — When port data was partially available, percentage denominator mixed wg0 and service data. Now uses consistent service-only denominator

## v1.2.0

**Base image:** `ghcr.io/hotio/base:alpinevpn@sha256:a3f171bc00b03218907c6bd6530e4231446def5ed2e8ebed53927ab2693919c7` (2026-02-23)

### Features
- **Traffic tab** — Live throughput graphs (1h/6h/24h/72h) with smooth bezier curves, DL/UL toggle, rate limit line, per-instance overlay lines
- **Stats tab** — Daily/weekly/monthly volume bar charts with stacked per-service breakdown, period selector (7d/30d/3m/6m/12m/All), auto-grouping, tooltips with percentages
- **qBit API integration** — Per-service traffic stats via qBittorrent WebUI API (replaces nft port counters)
- **Persistent statistics** — Ring buffer (72h) + daily volumes (365 days) + cumulative totals saved to disk, survive restarts
- **Always-visible status header** — Live speed, peak/avg 24h, volume 24h, active rule indicator
- **Per-service bar charts** — Stacked bars with total reference, custom colors via color picker, toggle visibility
- **Reset stats** — Clear all historical data for fresh start after adding port monitoring
- **Container hostname** — UI title and labels use container name automatically
- Collapsible config sections (Settings closed by default, Schedule open)
- Period totals table with per-service avg and percentage breakdown

## v1.1.0

**Base image:** `ghcr.io/hotio/base:alpinevpn@sha256:a3f171bc00b03218907c6bd6530e4231446def5ed2e8ebed53927ab2693919c7` (2026-02-23)

### Features
- **Web UI** — Configure schedule rules, default rates, and burst buffer from a browser (port 8090)
- Effective schedule summary — auto-generated text showing what rates apply when, with rule references
- Collapsible summary view
- Dual config source — edit via UI or manually in traffic.conf (newest file wins)
- UI config stored as JSON alongside bash config for lossless round-trips
- Input validation (time format, day names, rate bounds 0-10000 MB/s)
- Per-section save buttons (Settings + Schedule)

### CI
- `dev` tag built on every push to main (for testing)
- `latest` + semver tags only built on version tags
- `v` prefix on Docker tags (v1.1.0, v1.1, v1)

## v1.0.0

**Base image:** `ghcr.io/hotio/base:alpinevpn@sha256:a3f171bc00b03218907c6bd6530e4231446def5ed2e8ebed53927ab2693919c7` (2026-02-23)

### Features
- nftables bandwidth management (upload + download)
- Time-based scheduling with midnight carry-over
- Config hot-reload (10s polling)
- VPN reconnect recovery (verify watchdog every 60s)
- Configurable burst buffer for smooth TCP throughput
- Config version upgrade system (preserves user values on image update)
- Gap-tolerant schedule numbering (e.g., rules 1, 3, 5)
- Schedule-aware startup (applies correct rate on restart, not just defaults)

### Bug fixes
- **crond output lost** — Schedule triggers and verify watchdog output was silently discarded by BusyBox crond (no mail transport in Alpine). Fixed by redirecting crontab output to `/proc/1/fd/1` (Docker log).
- **Duplicate crond processes on config reload** — BusyBox `crond` daemonizes by default, so `$!` captured the parent PID which exits immediately. The `kill` on config reload targeted a dead process while the real daemon accumulated. Fixed by starting crond with `-f` (foreground mode).

### Notes
- Pinned to hotio base digest for reproducible builds
- To downgrade: use a specific version tag (e.g., `ghcr.io/prophetse7en/vpn-gateway:v1.0.0`)
