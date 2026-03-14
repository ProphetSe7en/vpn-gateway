# Changelog

## v1.0.0 (not yet tagged)

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
