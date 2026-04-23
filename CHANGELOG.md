# Changelog

## v1.4.1

Patch release for two issues surfaced right after v1.4.0 rolled out.

### Fixed

- **Saving any Settings panel no longer 400's when a monitored port has a masked secret.** Reported pattern: user changes VPN IP Badge dropdown → Save → "Validation error: port N (SABnzbd): API key must be entered, not the masked placeholder". Root cause: `handlePutConfig`'s unmask loop matched existing ports strictly by `Port` int. If the Port in the submitted body didn't round-trip cleanly (parsed as 0, or the user edited a port number in the same save), the lookup missed and the `********` sentinel reached `ValidateConfig`, which correctly rejected it. Fix: three-tier match in the unmask loop — Port int → Name → positional index. Any port with a matching prior entry gets its masked credentials resolved before validation runs. New ports (no match on any strategy) still fail validation as intended, so the "genuinely-new-port-with-mask" error path is preserved.

### Changed

- **Toast notifications moved from bottom-right to top-center.** Matches Clonarr's pattern. Bottom-right was easy to miss on wide screens when the user's focus was mid-page — top-center puts success/error feedback right where the user is already looking. No behaviour change beyond position + slightly larger shadow for visibility against the main content.

## v1.4.0

**⚠️ Breaking change:** Authentication is now enabled by default (Forms + "Disabled for Trusted Networks", matching the Radarr/Sonarr pattern). On first run after upgrade, vpn-gateway redirects to `/setup` to create an admin username and password. Homepage widgets hitting `/api/stats/widget` continue to work without auth (the widget endpoint is explicitly public — no secrets, no enumeration); other `/api/*` endpoints now require the API key (Settings → Security) sent as `X-Api-Key` header.

### Added

- **Authentication (Radarr/Sonarr pattern)** — `/config/auth.json` stores the bcrypt-hashed password + API key. Three modes:
  - `forms` (default): login page + session cookie, 30-day TTL
  - `basic`: HTTP Basic behind a reverse proxy
  - `none`: auth disabled (requires password-confirm to enable — catastrophic blast radius)
- **Authentication Required** — `enabled` (every request needs auth) or `disabled_for_local_addresses` (default — LAN bypasses)
- **Trusted Networks** — user-configurable CIDR list of what counts as "local". Empty = Radarr-parity defaults (10/8, 172.16/12, 192.168/16, link-local, IPv6 ULA, loopback). Narrow the list (`192.168.86.0/24`, `192.168.86.22/32`) for tighter control
- **Trusted Proxies** — required when vpn-gateway sits behind a reverse proxy so `X-Forwarded-For` is trusted
- **Env-var override for trust-boundary config** — set `TRUSTED_NETWORKS` and/or `TRUSTED_PROXIES` in the Unraid template or `docker-compose.yml` to pin values at host level. When set, the UI shows the field as locked and rejects edits — the trust boundary can only be changed by editing the template and restarting
- **API key** — auto-generated on first setup, rotatable from the Security panel. Send as `X-Api-Key: <key>` header (preferred) or `?apikey=<key>` query param (legacy — leaks to access logs). For Homepage widgets, scripts, Uptime Kuma
- **Change password** — from the Security panel. Requires current password. Invalidates all other sessions
- **CSRF protection** — double-submit cookie pattern on all state-mutating requests. Transparent to browser users; API key requests bypass
- **Security headers** — `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`
- **Security panel in Settings** — mode/Required dropdowns, Trusted Networks + Trusted Proxies inputs (disabled + amber banner when env-locked), Session TTL, API key view/copy/regenerate, Change Password, Disable-auth modal (requires current password), no-auth warning banner at top of UI when `authentication=none`
- **Credential masking** on `/api/config` — qBittorrent passwords, Dispatcharr password, SABnzbd API key round-trip as `********`. Empty-on-unchanged-edit preserves stored value on save
- **Public health endpoint** — `/api/health` returns `{"ok":true}` with no auth, for Docker HEALTHCHECK / Uptime-Kuma / reverse-proxy probes
- **Public stats widget** — `/api/stats/widget` stays public so existing Homepage installs keep working after upgrade (same risk profile as `/api/health` — no secrets, no enumeration)
- **qBit port auto-sync** — replaces the `claabs/qbittorrent-port-forward-file` sidecar. When the PIA/Proton tunnel rotates the forwarded port, vpn-gateway reads `/config/wireguard/forwarded_port` every 30 s and pushes the new value to the opt-in qBit instance via `setPreferences`. Validator enforces at most one qBit instance with auto-sync enabled (PIA gives one port per tunnel). Last-synced value persisted in history so re-enables don't re-push a stale port. Per-port auth backoff: 3 consecutive auth failures triggers a 15-minute cooldown so we can't trigger a qBit IP-ban or spam the log
- **qBittorrent Username + Password** — qBit instances with `LocalHostAuth=true` (the default for most setups) now authenticate via the usual login flow. Per-port SID-cookie cache, 401 retry on cookie expiry, dedicated 15 s write-client for `setPreferences` calls. The empty-credentials path is preserved — instances with `LocalHostAuth=false` continue to work without authentication
- **VPN IP badges on dashboard** — shows the server-side exit IP (parsed from `[Peer] Endpoint` in `wg0.conf`, `wg show` preferred when available). Optional second badge for the internal tunnel IP (10.x, parsed from `ip -4 addr show wg0`). Per-badge toggle: Off / Server / Tunnel / Both. Works with PIA, Proton, TorGuard, AirVPN, generic WireGuard — any provider whose hotio-managed `wg0.conf` has a standard `[Peer] Endpoint` line
- **Forwarded Ports on dashboard** — lists ports visible to vpn-gateway from two sources, union displayed: static `$VPN_PORT_REDIRECTS` env var (TorGuard / Mullvad-PF / AirVPN / generic, set in Unraid template) and dynamic `/config/wireguard/forwarded_port` file (PIA auto-PF + Proton NAT-PMP, hotio writes + refreshes). Per-source badge labels it as static or auto. Display mode: Off / Auto (hide when empty) / All (always show). Hide the whole card when both sources empty (Mullvad without PF)
- **IP/Port info row under card-title** — condenses exit IP, tunnel IP, and forwarded ports into a single compact row on the dashboard header with live change-tracking (flash-highlight on port rotation)
- **UI Scale toggle** — Settings → Tools → UI Scale (Compact 100% / Default 110% / Large 120%). Applies to the entire app including graphs and tooltips via `document.documentElement.style.zoom`. Persisted per-browser in localStorage so different devices can pick independently

### Changed

- **Base image** — hotio/base:alpinevpn bumped from the 2026-02-23 pin to 2026-04-17 (Alpine 3.21→3.22 refresh + service-pia / service-healthcheck 4-space indent fix upstream)
- **Go toolchain** — 1.25 + toolchain 1.25.9
- **Go UI package layout** — flat `ui/` split into `ui/{auth,netsec}/` subpackages; `safego` / `atomic` helpers for panic recovery and safe file writes
- **Cookie names** — new `vpngw_session` + `vpngw_csrf` (no prior sessions exist for auth, so no break)
- **Browser autofill suppression on credential fields** — qBit / Dispatcharr / SABnzbd password inputs carry `autocomplete="new-password"` + `data-1p-ignore` + `data-lpignore` + `data-bwignore` so Chrome + 1Password + LastPass + Bitwarden stop offering to save or prefill them (they were offering credentials from random unrelated sites)

### Security

- First-run forces the `/setup` wizard — no default credentials
- bcrypt cost 12; password verify is timing-equalized (prevents user-enumeration via response timing)
- Session persistence via atomic write to `/config/sessions.json` (survives container restart). Session cleanup goroutine wrapped in `safeGo` so a panic can't kill the process
- CIDR min-mask enforced (`/8` IPv4, `/16` IPv6) to reject mis-typed host bits masking as subnets
- Atomic writes on all state files (`/config/.traffic-ui.json` at 0600, `/config/.traffic-stats.json` at 0644) with crypto/rand-suffixed tmp + rename (trap T71) so two concurrent writers can't corrupt each other
- Log-injection guards on user-supplied strings that reach `log.Printf`
- `Cache-Control: no-store` on every `/api/*` response
- Generic 400 on JSON parse errors — raw `json.Unmarshal` error strings leaked byte offsets and field names in prior versions
- Concurrent PUT `/api/config` serialized with a mutex so two admin saves can't lose each other's updates
- Full audit trail in `docs/security-implementation-baseline.md` T1–T80

### Fixed

- **First-boot race** — `svc-webui` now depends on `svc-traffic` in s6-rc so the UI process cannot start before `traffic.conf` is seeded. Previously a lucky scheduler ordering produced a crash-loop on fresh installs
- **Env-lock reject-on-no-change** — partial PUTs (Bandwidth / Schedule / Security saves) no longer 403 when `TRUSTED_NETWORKS` / `TRUSTED_PROXIES` env var is set but the on-disk value differs. Env-locked fields now accept submissions that match EITHER the effective value OR the existing on-disk value
- **Test button for saved ports** — `POST /api/test-port` now resolves the `********` credential-mask sentinel by looking up the port in the on-disk config. Before: every "Test" click on a saved entry failed with auth error, even when stored credentials were correct
- **Session-expiry mid-edit** — saves, API-key regeneration, and password change now redirect to `/login` on 401 instead of showing a generic "Failed to save" toast. Handled centrally in the fetch wrapper so ~50 API call sites across the UI inherit the behavior without per-call boilerplate
- **Alpine `:style` vs `x-show` conflict** — an initial implementation of the VPN IP badge used `:style="... ? 'margin-left:auto' : ''"` which replaced the entire inline style attribute and wiped the `display:none` that `x-show` sets. Fixed by switching to object syntax `{marginLeft: ...}` so Alpine sets `element.style.marginLeft` directly without touching `display`
- **VPN IP semantic correction** — a pre-release draft of the VPN IP badge showed `10.13.128.x` (our client-side WireGuard address, read from `ip addr show wg0`). Users expect "VPN IP" to mean the server-side exit IP visible externally. Final implementation parses `Endpoint` from the `[Peer]` section of `wg0.conf` (or `wg show` when available), which is what hotio logs on startup
- **Static forwarded-port display** — first cut showed `55000→55000/tcp` per `VPN_PORT_REDIRECTS` entry. Container port + protocol are Docker implementation details and noise for the dashboard — now just the VPN port number, symmetric with the dynamic-port display

### Notes for upgraders

- First boot redirects to `/setup`. Choose a strong password (≥10 chars, 2+ of upper/lower/digit/symbol)
- If you access vpn-gateway from the same LAN the host is on, the default "Disabled for Trusted Networks" mode will skip login for you — no change in day-to-day UX
- Homepage widget pointing at `/api/stats/widget` keeps working — endpoint stays public
- Homepage widget pointing at any other `/api/*` endpoint: add the API key from Settings → Security, send as `X-Api-Key` header
- Lost your password: stop the container, delete `/config/auth.json` (credentials only — no schedule / bandwidth data), restart. The setup wizard will run again
- Env-lock your trust boundary in the Unraid template: add `TRUSTED_NETWORKS` with your LAN CIDR and `TRUSTED_PROXIES` with your reverse-proxy IP. UI will show both fields as locked with an amber banner so misconfigs are obvious

## v1.3.0

### Features
- **Multi-service monitoring** — SABnzbd and Dispatcharr pollers alongside qBittorrent. ServicePoller interface with registry pattern for extensibility
- **Dispatcharr integration** — JWT authentication, Active Streams panel on Traffic tab showing channel names/clients/codec, credential masking on API responses
- **SABnzbd integration** — Session byte tracking from server_stats deltas, gap detection for downtime handling
- **nft byte counters for Dispatcharr** — Real-time 3s traffic updates using nftables counters instead of API polling. Smooth graphs without impacting Dispatcharr's stream delivery
- **Custom confirm modal** — Replaces native browser confirm() dialogs

### Improvements
- **Dispatcharr API throttling** — Active Streams panel polls API every 30s; traffic data uses nft counters with no API calls
- **Period Summary explanation** — Simplified universal text explaining VPN total vs per-service difference
- **Service Monitoring settings** — Clarified port field with description text and tooltip
- **8 KiB body cap** on `/api/test-port` (DoS hardening)
- **Multi-arch builds** (amd64 + arm64 via QEMU)

## v1.2.14

### Features
- **Homepage widget endpoint** — New `/api/stats/widget` returns pre-formatted stats for [Homepage](https://gethomepage.dev/) dashboards. Fields: `dlSpeed`, `ulSpeed`, `totalDl`, `totalUl`, `dailyDl`, `dailyUl`. Values are human-readable (e.g., "23.0 MB/s", "6.95 TB") — no client-side formatting needed.

## v1.2.13

### Improvements
- **UI remembers last tab** — Selected tab (Config/Traffic/Stats) persists across page reloads via localStorage

## v1.2.12

### Bug fixes
- **Slow container shutdown (10s → 3s)** — svc-traffic's EXIT trap ran `sleep infinity` on every exit, including normal shutdowns. Docker waited the full 10s stop timeout before force-killing. Now exits cleanly on SIGTERM with proper nftables cleanup

### Improvements
- **Setup guide: Extra Parameters cleanup** — Added warning to remove `--hostname` and `--cap-add=NET_ADMIN` from qBittorrent containers when using container network mode. `--hostname` prevents startup, `NET_ADMIN` is only needed on vpn-gateway
- **Setup guide: restart troubleshooting** — Documented Docker limitation where dependent containers lose network after gateway restart. Added Unraid workaround (force recreate via dummy edit) and Docker Compose `depends_on` solution

## v1.2.11

### Bug fixes
- **UI showed container ID as title** — Users without `--hostname` set saw a hex container ID in the header instead of the product name. Now always shows "VPN Gateway"

### Improvements
- **Pinned version as default** — Unraid template now defaults to a pinned version tag instead of `latest`, preventing unexpected updates that can take down containers routed through the gateway
- **README uses pinned tags** — All examples and install instructions use pinned version tags with a prominent warning against using `latest`

## v1.2.10

### Bug fixes
- **Stats tooltip showed 0** — Tooltip on Stats tab download/upload charts showed "Total 0" when only VPN total was visible (no qBit ports configured or toggled off). Was summing port values instead of using actual VPN total.

## v1.2.9

### Features
- **Built-in Docker healthcheck** — Verifies WireGuard tunnel (peer handshake) and web UI (`/api/stats/latest`) every 30s. No more need for `--health-cmd` in Extra Parameters

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
