# Security Policy

## Supported versions

| Version | Security updates |
|---------|------------------|
| `v1.4.0` (latest) and later | ✅ Yes |
| Earlier `v1.x` releases | ❌ No — please upgrade (v1.4.0 is the first release with authentication) |

## Reporting a vulnerability

**Please do NOT open a public GitHub issue for security bugs.** Even describing an attack path in a public forum before a fix ships puts other users at risk.

### Preferred channel

Email: **eirik.svortevik@gmail.com** with subject line `[vpn-gateway Security] <brief summary>`.

### Fallback

If email fails or you need pseudonymous submission, use GitHub's private **Report a vulnerability** link on the [repository security tab](https://github.com/ProphetSe7en/vpn-gateway/security/advisories/new).

### What to include

- vpn-gateway version (from the UI footer or `GET /api/version`)
- Clear reproduction steps (command + request body + expected vs actual response is ideal)
- Impact assessment — what data/access can the attacker obtain?
- Your disclosure timeline preference

### What to expect

- **Acknowledgement within 72 hours** of receipt (usually faster — solo maintainer, best-effort).
- **Triage and severity assessment within 7 days.** I'll confirm whether I accept the finding, classify severity, and propose a fix + disclosure timeline.
- **Fix within 14 days** for Critical/High findings. Medium/Low may take a release cycle.
- **Coordinated disclosure** — I'll ship a patched release first, then credit you in the CHANGELOG and this document (unless you prefer anonymity). Please do not publish details before the patch ships.

### How I handle reports

- Reporter credit in CHANGELOG + this document by default (anonymous on request).
- Honest acknowledgement when a report is valid — including in the CHANGELOG.
- Open to public discussion of a finding after the patch ships.

## Security model

vpn-gateway is a **local admin tool** for shaping and monitoring VPN traffic. The design assumes:

- You control the host where it runs.
- You do not expose port 6050 directly to the internet without a reverse proxy.
- You protect `/config/` the same way you protect WireGuard's `wg0.conf` (file permissions, backup encryption, LUKS on the host).

### What vpn-gateway does

- **Authentication required by default** (Forms mode, bcrypt cost 12). First-run setup wizard forces admin account creation — no default credentials.
- **Three auth modes** — Forms (default), HTTP Basic (reverse-proxy friendly), None (requires password-confirm to enable; catastrophic blast radius).
- **Trusted Networks** — Radarr/Sonarr-parity default CIDR list. Narrow to your LAN (`192.168.x.0/24`) or pin to a specific admin host (`192.168.x.22/32`) for tighter control.
- **CSRF protection** on all state-mutating endpoints (double-submit cookie pattern).
- **Security headers**: `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`.
- **Credential masking** on `/api/config` — qBit password, Dispatcharr password, SAB API key round-trip as `********`. Empty-on-unchanged-edit preserves the stored value on save.
- **Session persistence** to disk (atomic write, survives container restart).
- **File permissions**: `/config/.traffic-ui.json` (secrets) written with mode `0600`; `/config/auth.json` `0600` in dir `0700`.
- **X-Forwarded-For hardening**: only trusted when the direct peer IP matches a configured Trusted Proxy. Rightmost-non-trusted algorithm defeats leftmost-spoofing.
- **Env-var override** for trust-boundary config (`TRUSTED_NETWORKS`, `TRUSTED_PROXIES`) — pin at host level to defend against UI-takeover attackers.
- **Public endpoints (by design)** — `/api/health` (liveness probe, no data) and `/api/stats/widget` (formatted aggregate dl/ul + totals for Homepage dashboards; same risk profile as `/api/health`). Everything else requires auth.
- **Concurrency** — `PUT /api/config` is serialized per admin so two simultaneous saves cannot lose each other's updates. Session persistence is atomic tmp+rename with crypto/rand suffix (trap T71) so concurrent writers cannot corrupt the snapshot.

### What vpn-gateway does NOT do (by design)

- **Encryption at rest of poller credentials.** qBit password, Dispatcharr password, SAB API key are stored plaintext in `/config/.traffic-ui.json` (mode 0600). Same trust model as Radarr/Sonarr themselves — both of those also store their API keys plaintext in `config.xml`. If an attacker has read access to `/config/`, any local keystore key would be readable from the same filesystem. If you need encryption-at-rest against backup-disk exfiltration, open a GitHub issue — it's a reasonable v1.x+ feature.
- **Rate limiting on `/login`.** Delegated to the reverse proxy (fail2ban, CrowdSec, SWAG plugins). Basic-auth mode is a CPU-amplification vector without rate-limit — prefer Forms mode or front vpn-gateway with a proxy that handles auth.
- **Account lockout.** Same reasoning as rate limiting.
- **Audit log of admin actions.** The Docker event stream + reverse-proxy access logs cover request-level history. If you need per-action audit, a future release can add it — open an issue.
- **TLS termination.** Runs plain HTTP on port 6050. Use a reverse proxy (SWAG, Traefik, Caddy, NPM) for TLS — configure `TRUSTED_PROXIES` so `X-Forwarded-Proto: https` is honored for Secure cookies.

## Security audit trail

vpn-gateway's security implementation is backed by an internal trap catalogue (T1–T80) — every finding from past code reviews is preserved with the mitigation pattern and the reason it was flagged. Covers auth primitives, middleware wiring, credential masking, CSRF, security headers, race conditions, info leakage, log injection, and supply-chain concerns. Requests for access to specific trap rationale can be made via the disclosure email above.

Current CI: `go test -race ./...` + `govulncheck ./...` run on every push and PR against `main`.

## Changelog of security-relevant changes

See `CHANGELOG.md` — entries in the **Security** sections or explicitly referencing trap IDs (T1–T80).
