# Changelog

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
