package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"vpn-gateway-ui/auth"
	"vpn-gateway-ui/netsec"
)

// Version is set at build time via ldflags
var Version = "dev"

//go:embed static
var staticFiles embed.FS

type App struct {
	configPath    string
	iface         string // VPN tunnel interface name, e.g. "wg0"
	vpnConfigPath string // WireGuard config file, e.g. "/config/wireguard/wg0.conf"
	traffic       *TrafficCollector
	authStore     *auth.Store
	history       *HistoryStore
	// putConfigMu serializes handlePutConfig so the read-modify-write of
	// auth.Store config + traffic config cannot lose updates when two
	// admins save concurrently. All other handlers are read-only or
	// touch independent state, so contention is bounded to the PUT path.
	putConfigMu sync.Mutex
}

func main() {
	port := os.Getenv("UI_PORT")
	if port == "" {
		port = "6050"
	}

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/traffic.conf"
	}

	iface := os.Getenv("INTERFACE")
	if iface == "" {
		iface = "wg0"
	}

	// WireGuard config path — default /config/wireguard/<VPN_CONF>.conf to
	// match hotio's layout. Readable without container exec; parsed on
	// demand to surface the VPN-server endpoint (exit IP) in /api/status.
	vpnConf := os.Getenv("VPN_CONF")
	if vpnConf == "" {
		vpnConf = iface
	}
	// Derive the wireguard dir from the app config's parent (/config/
	// traffic.conf → /config/wireguard/). This matches hotio's convention
	// without requiring a separate env var.
	vpnConfigPath := filepath.Join(filepath.Dir(configPath), "wireguard", vpnConf+".conf")

	// Start traffic collector
	traffic := NewTrafficCollector(iface, configPath)
	ctx, cancel := context.WithCancel(context.Background())
	safeGo("traffic-collector", func() { traffic.Run(ctx) })

	historyPath := filepath.Join(filepath.Dir(configPath), ".traffic-ipport-history.json")
	app := &App{
		configPath:    configPath,
		iface:         iface,
		vpnConfigPath: vpnConfigPath,
		traffic:       traffic,
		history:       NewHistoryStore(historyPath),
	}

	mux := http.NewServeMux()

	// Config API routes
	mux.HandleFunc("GET /api/status", app.handleStatus)
	mux.HandleFunc("GET /api/config", app.handleGetConfig)
	mux.HandleFunc("PUT /api/config", app.handlePutConfig)

	// Stats API routes
	mux.HandleFunc("GET /api/stats/stream", app.handleStatsSSE)
	mux.HandleFunc("GET /api/stats/history", app.handleStatsHistory)
	mux.HandleFunc("GET /api/stats/latest", app.handleStatsLatest)
	mux.HandleFunc("GET /api/stats/widget", app.handleStatsWidget)
	mux.HandleFunc("GET /api/stats/daily", app.handleStatsDailyVolume)
	mux.HandleFunc("POST /api/stats/reset", app.handleStatsReset)

	// Service poller routes (Phase 1+: per-service details and test)
	mux.HandleFunc("GET /api/service-details", app.handleServiceDetails)
	mux.HandleFunc("POST /api/test-port", app.handleTestPort)

	// Health endpoint — public (in auth.publicExact). Used by Docker HEALTHCHECK
	// and external monitors. No data, no enumeration risk — liveness only.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	// ==== Authentication =====================================================
	authStore, authHandlers := initAuth(ctx, app)
	app.authStore = authStore

	mux.HandleFunc("GET /setup", authHandlers.handleSetupPage)
	mux.HandleFunc("POST /setup", authHandlers.handleSetupSubmit)
	mux.HandleFunc("GET /login", authHandlers.handleLoginPage)
	mux.HandleFunc("POST /login", authHandlers.handleLoginSubmit)
	mux.HandleFunc("POST /logout", authHandlers.handleLogout)
	mux.HandleFunc("GET /api/auth/status", authHandlers.handleAuthStatus)
	mux.HandleFunc("GET /api/auth/api-key", authHandlers.handleGetAPIKey)
	mux.HandleFunc("POST /api/auth/regenerate-api-key", authHandlers.handleRegenAPIKey)
	mux.HandleFunc("POST /api/auth/change-password", authHandlers.handleChangePassword)

	// Static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("Failed to create static file system: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// Background: reap expired sessions every 5 min
	safeGo("session-cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				authStore.CleanupExpiredSessions()
			}
		}
	})

	// qBit port auto-sync: watch /config/wireguard/forwarded_port and push
	// the current value into the qBit instance flagged with
	// AutoSyncForwardedPort. Replaces the claabs/qbittorrent-port-forward-
	// file sidecar for users on dynamic-PF providers (PIA, Proton).
	//
	// Polls every 30 s — matches the cadence at which a PIA rotation can
	// realistically reach us (PIA renews far less often) and avoids
	// hammering qBit's API on every status tick. Config is re-read each
	// time so toggling the per-instance flag in the UI takes effect on
	// the next iteration without a container restart.
	safeGo("qbit-port-autosync", func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				app.runQBitPortAutoSync()
			}
		}
	})

	// Middleware chain — outermost first:
	//   SecurityHeaders → CSRF → Auth → mux
	var handler http.Handler = authStore.Middleware(mux)
	handler = authStore.CSRFMiddleware(handler)
	handler = auth.SecurityHeadersMiddleware(handler)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE needs unlimited write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("Web UI listening on :%s", port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-done
	log.Println("Shutting down...")
	cancel() // Stop traffic collector (triggers save)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
}

// initAuth loads auth settings from the current config, validates, loads
// existing credentials from /config/auth.json, and returns the store +
// handlers ready to wire into the mux.
//
// Refuses to start (log.Fatal) on unsafe combinations or malformed auth.json.
func initAuth(ctx context.Context, app *App) (*auth.Store, *AuthHandlers) {
	cfg := auth.DefaultConfig()

	// loadConfig returns (cfg, nil) on first-run (no JSON yet, parseBashConfig
	// falls back to defaults). Any OTHER error — corrupted file, permission
	// denied, malformed JSON — must be fatal so we don't silently drop the
	// admin's configured TRUSTED_NETWORKS / TRUSTED_PROXIES and fall back to
	// Radarr-parity defaults. An attacker with file-write access must not be
	// able to trigger a trust-boundary reset by corrupting config.
	appCfg, err := loadConfig(app.configPath)
	if err != nil {
		// loadConfig only surfaces errors if BOTH bash and JSON paths failed.
		// That means the config dir is unreadable or both files are corrupt —
		// refuse to start rather than silently run with defaults.
		log.Fatalf("auth: load app config: %v", err)
	}
	if appCfg != nil {
		if appCfg.Authentication != "" {
			cfg.Mode = auth.AuthMode(appCfg.Authentication)
		}
		if appCfg.AuthenticationRequired != "" {
			cfg.Requirement = auth.Requirement(appCfg.AuthenticationRequired)
		}
		if appCfg.SessionTTLDays > 0 {
			cfg.SessionTTL = time.Duration(appCfg.SessionTTLDays) * 24 * time.Hour
		}
		if appCfg.TrustedNetworks != "" {
			nets, err := netsec.ParseTrustedNetworks(appCfg.TrustedNetworks)
			if err != nil {
				log.Fatalf("auth: invalid trustedNetworks config: %v", err)
			}
			cfg.TrustedNetworks = nets
		}
		if appCfg.TrustedProxies != "" {
			ips, err := netsec.ParseTrustedProxies(appCfg.TrustedProxies)
			if err != nil {
				log.Fatalf("auth: invalid trustedProxies config: %v", err)
			}
			cfg.TrustedProxies = ips
		}
	}

	// Env-var override for trust-boundary config. If the env var is set at
	// process start, that value wins over the config-file value AND the UI
	// cannot change it. Use in Unraid templates / docker-compose to lock
	// down the trust boundary against UI-takeover attacks.
	if envNets := strings.TrimSpace(os.Getenv("TRUSTED_NETWORKS")); envNets != "" {
		nets, err := netsec.ParseTrustedNetworks(envNets)
		if err != nil {
			log.Fatalf("auth: invalid TRUSTED_NETWORKS env var: %v", err)
		}
		cfg.TrustedNetworks = nets
		cfg.TrustedNetworksLocked = true
		cfg.TrustedNetworksRaw = envNets
		log.Printf("auth: trusted_networks locked by TRUSTED_NETWORKS env var (%d entries)", len(nets))
	}
	if envProxies := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); envProxies != "" {
		ips, err := netsec.ParseTrustedProxies(envProxies)
		if err != nil {
			log.Fatalf("auth: invalid TRUSTED_PROXIES env var: %v", err)
		}
		cfg.TrustedProxies = ips
		cfg.TrustedProxiesLocked = true
		cfg.TrustedProxiesRaw = envProxies
		log.Printf("auth: trusted_proxies locked by TRUSTED_PROXIES env var (%d entries)", len(ips))
	}

	if err := auth.ValidateConfig(cfg); err != nil {
		log.Fatalf("auth config refuses to start: %v", err)
	}

	store := auth.NewStore(cfg)
	if _, err := store.Load(); err != nil {
		log.Fatalf("auth: load credentials: %v", err)
	}

	if store.IsConfigured() {
		log.Printf("auth: mode=%s required=%s user=%s", cfg.Mode, cfg.Requirement, store.Username())
	} else {
		log.Printf("auth: no credentials yet — first run, /setup wizard will prompt for admin user")
	}

	if cfg.Mode == auth.ModeNone {
		log.Printf("auth: WARNING — authentication is DISABLED via authentication=none. Do not expose this container to untrusted networks.")
	}

	// Periodic loud warning while in none mode. Picks up live-reload both ways.
	safeGo("auth-none-warning", func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if store.Config().Mode == auth.ModeNone {
					log.Printf("auth: WARNING — authentication is still DISABLED. Every request is admin. Re-enable auth or restrict to 127.0.0.1.")
				}
			}
		}
	})

	return store, &AuthHandlers{Store: store}
}

// --- API Handlers ---

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// GET /api/status — current rates, active rule, nft counters
func (app *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to read config: %v", err))
		return
	}

	// Get nft status by reading counters
	upRule, downRule := getNftCounters()

	// Resolve which rule is currently active
	activeRule := resolveActiveRule(cfg)

	exitIP, exitPort := resolveVPNEndpoint(app.iface, app.vpnConfigPath)
	tunnelIP := getTunnelIP(app.iface)

	// Forwarded-port data from three sources — the frontend filters
	// visibility by config.forwardedPortsDisplay, backend always sends.
	// Cheap to compute: env var read + two optional file reads with
	// graceful zero-value fallback when a provider doesn't populate them.
	wireguardDir := filepath.Dir(app.vpnConfigPath)
	staticPorts := parseStaticPortRedirects(os.Getenv("VPN_PORT_REDIRECTS"))
	dynamicPort := readDynamicForwardedPort(wireguardDir)
	rotatesAt := readPIARotatesAt(wireguardDir, app.iface)
	var rotatesAtStr string
	if !rotatesAt.IsZero() {
		rotatesAtStr = rotatesAt.UTC().Format(time.RFC3339)
	}

	// Reconcile current observations with persisted history so the UI
	// can flag "this just changed" without lying after a container
	// restart. Empty current values (interface down, file missing)
	// preserve the last-known entry — we don't overwrite with absence.
	hist := app.history.Update(exitIP, tunnelIP, dynamicPort)

	status := map[string]any{
		"enabled":         cfg.Enabled,
		"scheduleEnabled": cfg.ScheduleEnabled,
		"defaultDown":     cfg.DefaultDown,
		"defaultUp":       cfg.DefaultUp,
		"burstMs":         cfg.BurstMs,
		"rules":           cfg.Rules,
		"activeUp":        upRule,
		"activeDown":      downRule,
		"activeRule":      activeRule,
		"hostname":        "VPN Gateway",
		"version":         Version,
		// Exit IP — the public IP of the VPN server, read from the peer
		// Endpoint line in wg0.conf. This is the IP external services see
		// when our traffic exits the tunnel. Primary "VPN IP" for users.
		"exitIp":          exitIP,
		"exitPort":        exitPort,
		"exitIpPrevious":  hist.ExitIP.Previous,
		"exitIpChangedAt": iso(hist.ExitIP.ChangedAt),
		// Tunnel IP — the IP the provider assigned on OUR side of the
		// tunnel (wg0 interface). Secondary info for debugging / routing
		// verification. Empty when wg0 is down.
		"tunnelIp":          tunnelIP,
		"tunnelIface":       app.iface,
		"tunnelIpPrevious":  hist.TunnelIP.Previous,
		"tunnelIpChangedAt": iso(hist.TunnelIP.ChangedAt),
		// Forwarded ports. static[] is parsed from VPN_PORT_REDIRECTS env
		// var (TorGuard / generic / Mullvad-PF). dynamic is the current
		// PIA / Proton auto-PF port from /config/wireguard/forwarded_port
		// (0 when no auto-PF active). rotatesAt is the RFC3339 expiry
		// timestamp from hotio's PIA persist file when
		// VPN_PIA_PORT_FORWARD_PERSIST=true (empty otherwise).
		// dynamicPrevious + dynamicChangedAt let the UI flag a fresh
		// PIA rotation in amber for 48h with the prior port in the
		// tooltip — static ports are admin-managed and not tracked.
		"forwardedPortsStatic":            staticPorts,
		"forwardedPortsDynamic":           dynamicPort,
		"forwardedPortsRotatesAt":         rotatesAtStr,
		"forwardedPortDynamicPrevious":    hist.ForwardedPortDynamic.Previous,
		"forwardedPortDynamicChangedAt":   iso(hist.ForwardedPortDynamic.ChangedAt),
	}

	writeJSON(w, status)
}

// runQBitPortAutoSync executes one iteration of the auto-sync loop:
// load config, find the qBit instance opted in via AutoSyncForwardedPort,
// read the current dynamic forwarded port, and push it into qBit if it
// differs from what we last synced. No-op when:
//   - no PortMapping has the flag enabled
//   - no dynamic port is published by hotio yet (PIA/Proton not connected,
//     or provider doesn't do dynamic PF — TorGuard etc.)
//   - the port hasn't changed since the last successful sync
//
// All errors are logged but not returned — the next tick retries.
func (app *App) runQBitPortAutoSync() {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		log.Printf("qbit-port-autosync: load config: %v", err)
		return
	}
	var target *PortMapping
	for i := range cfg.Ports {
		pm := &cfg.Ports[i]
		if !pm.AutoSyncForwardedPort {
			continue
		}
		if pm.Type != "" && pm.Type != "qbittorrent" {
			continue
		}
		target = pm
		break
	}
	if target == nil {
		// No instance opted in. Clear persisted state so a later re-enable
		// on the same port (with the same forwarded port) triggers a fresh
		// sync — otherwise the equality check below would short-circuit and
		// the user would see "nothing happened" with no feedback.
		if app.history.AutoSyncedQBitPort() != 0 {
			app.history.SetAutoSyncedQBitPort(0)
		}
		return
	}
	wireguardDir := filepath.Dir(app.vpnConfigPath)
	currentPort := readDynamicForwardedPort(wireguardDir)
	if currentPort == 0 {
		return
	}
	previous := app.history.AutoSyncedQBitPort()
	if previous == currentPort {
		return
	}
	qbitP, ok := pollerFor("qbittorrent").(*qBitPoller)
	if !ok || qbitP == nil {
		log.Printf("qbit-port-autosync: qBit poller not registered — skipping")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := qbitP.SetListeningPort(ctx, *target, currentPort); err != nil {
		log.Printf("qbit-port-autosync: setPreferences port=%d on %s: %v", currentPort, sanitizeLogField(target.Name), err)
		return
	}
	app.history.SetAutoSyncedQBitPort(currentPort)
	log.Printf("qbit-port-autosync: pushed listen_port=%d to %s (was %d)",
		currentPort, sanitizeLogField(target.Name), previous)
}

// iso formats a time as RFC3339 UTC, returning "" for the zero value.
// Used for /api/status timestamp fields where empty == "no event".
func iso(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// getTunnelIP returns the first non-link-local IPv4 address assigned to
// the VPN tunnel interface (wg0 by default). Empty string when the
// interface is down, missing, or has only IPv6 / link-local addresses.
//
// This is our CLIENT-side address inside the tunnel — same value hotio
// logs as `[VPN] Added IPv4 address [...] to interface [wg0]` at startup.
// NOT the public exit IP. Use readWireguardEndpoint for that.
func getTunnelIP(iface string) string {
	if iface == "" {
		return ""
	}
	ni, err := net.InterfaceByName(iface)
	if err != nil {
		return ""
	}
	addrs, err := ni.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipn.IP.To4()
		if v4 != nil && !v4.IsLinkLocalUnicast() {
			return v4.String()
		}
	}
	return ""
}

// resolveVPNEndpoint returns the VPN server's public (ip, port), preferring
// the LIVE kernel state from `wg show` over static config-file parsing.
// This matters for PIA specifically: hotio regenerates wg0.conf on every
// container start, and if VPN_PIA_PREFERRED_REGION is unset the chosen
// server (and therefore Endpoint) can differ from the previous start.
// `wg show` reflects the actually-connected peer post-handshake — same
// source hotio itself uses internally (init-wireguard/run line 189).
//
// Falls back to wg0.conf when `wg show` yields nothing: pre-handshake,
// `wg` binary missing, or interface never brought up.
func resolveVPNEndpoint(iface, vpnConfigPath string) (ip, port string) {
	if ip, port := readLiveEndpoint(iface); ip != "" {
		return ip, port
	}
	return readWireguardEndpoint(vpnConfigPath)
}

// readLiveEndpoint returns the live endpoint from `wg show <iface>`.
// Parses lines of the form "endpoint: <ip>:<port>" (IPv6 literals use
// bracket notation, same as the config-file parser handles). Returns
// ("", "") on any error — the caller falls back to file-based reading.
//
// iface is validated defensively: only [A-Za-z0-9_.-] allowed. This is
// belt-and-suspenders — iface comes from the INTERFACE env var set at
// container start, and exec.Command does not invoke a shell, so command
// injection is already prevented. But restricting the charset makes
// audit trivial and catches any future refactor that might pipe through
// `sh -c`.
func readLiveEndpoint(iface string) (ip, port string) {
	if iface == "" {
		return "", ""
	}
	for _, r := range iface {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if !ok {
			return "", ""
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wg", "show", iface).Output()
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "endpoint:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "endpoint:"))
		lastColon := strings.LastIndexByte(val, ':')
		if lastColon < 0 {
			continue
		}
		host := strings.TrimSuffix(strings.TrimPrefix(val[:lastColon], "["), "]")
		if net.ParseIP(host) == nil {
			continue
		}
		return host, val[lastColon+1:]
	}
	return "", ""
}

// readWireguardEndpoint parses the Endpoint line from wg0.conf's [Peer]
// section and returns (ip, port) as strings. This is the VPN server's
// public address — for all standard VPN providers (PIA, TorGuard, Proton,
// Mullvad, generic WireGuard) this is also the exit IP that external
// services see when tunneled traffic reaches the internet. The server NATs
// outbound traffic through its own IP.
//
// Returns ("", "") on:
//   - File missing or unreadable (pre-setup, wrong path, permissions)
//   - No Endpoint line in the config (malformed or stripped config)
//   - DNS-form endpoint like "myvpn.example.com:51820" — resolving DNS
//     inside a handler would block up to the resolver timeout; let the
//     dashboard show nothing rather than hang. DNS-form endpoints are
//     rare in consumer VPN configs (providers hand out IP literals).
//
// Parse is a simple line scan — the WireGuard config format is a
// minimal INI subset, no need for a full INI library.
func readWireguardEndpoint(path string) (ip, port string) {
	if path == "" {
		return "", ""
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	inPeer := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inPeer = strings.EqualFold(line, "[Peer]")
			continue
		}
		if !inPeer {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if !strings.EqualFold(key, "Endpoint") {
			continue
		}
		// val is "host:port" — split from the right to handle IPv6
		// literals like "[2001:db8::1]:51820".
		lastColon := strings.LastIndexByte(val, ':')
		if lastColon < 0 {
			return "", ""
		}
		host := strings.TrimSuffix(strings.TrimPrefix(val[:lastColon], "["), "]")
		p := val[lastColon+1:]
		// Only surface IP-literal hosts. DNS names would require lookup;
		// skip them to avoid blocking the handler.
		if net.ParseIP(host) == nil {
			return "", ""
		}
		return host, p
	}
	return "", ""
}

// credentialMask is the sentinel value sent to the frontend in place of
// stored passwords / API keys. The frontend treats it as "value unchanged"
// and the PUT handler restores the real value from disk on save. Eight
// characters so it visually reads as a fixed-length placeholder regardless
// of the real credential length.
const credentialMask = "********"

// GET /api/config — full parsed config with credentials masked.
//
// Auth is enforced by middleware; reaching here means the caller is an
// authenticated admin (session, Basic, API key, or local-bypass). We STILL
// mask credentials on the wire so they never round-trip through the UI —
// a forthcoming XSS in static/index.html would otherwise exfiltrate them.
func (app *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to read config: %v", err))
		return
	}
	for i := range cfg.Ports {
		if cfg.Ports[i].Password != "" {
			cfg.Ports[i].Password = credentialMask
		}
		if cfg.Ports[i].APIKey != "" {
			cfg.Ports[i].APIKey = credentialMask
		}
	}
	writeJSON(w, cfg)
}

// PUT /api/config — update config.
//
// Credential merge: if a port's Password / APIKey comes in as the
// credentialMask sentinel, look up the matching port in the on-disk
// config and restore the real value before saving. Auth fields reload
// the auth store in-process so Security-panel saves take effect without
// a container restart. Env-locked trust-boundary fields (TRUSTED_NETWORKS
// / TRUSTED_PROXIES set via env var) cannot be changed via this endpoint.
func (app *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	app.putConfigMu.Lock()
	defer app.putConfigMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // 64KB limit
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, fmt.Sprintf("Read body: %v", err))
		return
	}

	// Partial-update model: start from the existing on-disk config, then
	// overlay whatever fields the request body provides. Go's json.Unmarshal
	// only touches fields that appear in the JSON — so the Security panel
	// can send just `{authentication, sessionTtlDays, ...}` without wiping
	// rules/ports/enabled, and the Bandwidth panel can send everything
	// except auth fields without wiping them either.
	existing, existingErr := loadConfig(app.configPath)
	var cfg Config
	if existingErr == nil && existing != nil {
		cfg = *existing
	}
	if err := json.Unmarshal(bodyBytes, &cfg); err != nil {
		// Log the raw parse error server-side for debugging, but return
		// only a generic message to the client. The json package's error
		// strings include byte offsets and expected-type hints that act
		// as a body-structure oracle for an authenticated attacker.
		log.Printf("handlePutConfig: json unmarshal: %v", err)
		writeError(w, 400, "invalid JSON body")
		return
	}

	// Decode confirm_password separately — it's a transient field used to
	// gate the Authentication → none transition, not part of Config.
	var sideband struct {
		ConfirmPassword string `json:"confirm_password"`
	}
	_ = json.Unmarshal(bodyBytes, &sideband)

	// Restore masked credentials from the on-disk config before validating.
	// Still needed: the UI sends `credentialMask` sentinel for fields the
	// user didn't touch, so we swap in the real stored values.
	//
	// Match strategy (resilient to config drift):
	//   1. Primary: match by Port int — stable identifier for a running service.
	//   2. Fallback: match by Name — survives edits where the user changed the
	//      listening port number of a previously-configured instance. Without
	//      this fallback, any port-number edit on the same save as a benign
	//      Settings change (VPN IP badge, UI scale) would trip the
	//      "API key must be entered" validator because the lookup misses and
	//      the mask sentinel stays in the cfg going into ValidateConfig.
	//   3. Last resort: positional index — handles the case where the UI
	//      sends a Port=0 placeholder for a port it couldn't parse back out
	//      of the GET payload. Only applied when both Port and Name fail AND
	//      the index is in range for existing.Ports.
	if existing != nil {
		oldByPort := make(map[int]PortMapping, len(existing.Ports))
		oldByName := make(map[string]PortMapping, len(existing.Ports))
		for _, pm := range existing.Ports {
			oldByPort[pm.Port] = pm
			if pm.Name != "" {
				oldByName[pm.Name] = pm
			}
		}
		for i := range cfg.Ports {
			var old PortMapping
			var ok bool
			if cfg.Ports[i].Port != 0 {
				old, ok = oldByPort[cfg.Ports[i].Port]
			}
			if !ok && cfg.Ports[i].Name != "" {
				old, ok = oldByName[cfg.Ports[i].Name]
			}
			if !ok && i < len(existing.Ports) {
				old = existing.Ports[i]
				ok = true
			}
			if !ok {
				continue
			}
			if cfg.Ports[i].Password == credentialMask {
				cfg.Ports[i].Password = old.Password
			}
			if cfg.Ports[i].APIKey == credentialMask {
				cfg.Ports[i].APIKey = old.APIKey
			}
		}
	}

	// Security gate: disabling auth is a major privilege reduction. Require
	// the caller's current password before allowing the forms/basic → none
	// transition. Skipped on first-ever configuration (existing == nil or
	// existing.Authentication unset) since there's no prior state to demote
	// from — the /setup wizard is the entry point for that.
	if cfg.Authentication == "none" &&
		existing != nil &&
		existing.Authentication != "" &&
		existing.Authentication != "none" {
		if app.authStore == nil {
			writeError(w, 500, "auth store not initialised — refusing to disable authentication")
			return
		}
		if !app.authStore.VerifyPassword(app.authStore.Username(), sideband.ConfirmPassword) {
			writeError(w, 401, "Password incorrect — authentication not disabled")
			return
		}
	}

	// Env-lock enforcement: if trusted_networks / trusted_proxies were
	// locked at startup via env var, the UI cannot override them. Fail
	// closed with a 403 so the user sees a clear error (not a silent
	// rewrite that reverts after restart).
	//
	// Accept the submission when cfg matches EITHER the env-locked
	// effective value (user explicitly sent the locked value) OR the
	// existing on-disk value (partial-merge case where the request
	// body didn't include this field, so cfg came in as existing).
	// Without the existing-value exception, Bandwidth/Schedule PUTs
	// 403 forever once env-lock is set but the disk value differs
	// from the locked effective — initAuth does not persist env-locked
	// values back to disk, so disk and effective can legitimately
	// diverge. Only a deliberate change to something OTHER than disk
	// and effective is rejected. Empty-string submission is still
	// blocked by the same predicate (empty ≠ effective ≠ existing when
	// the user has a stored value).
	if app.authStore != nil {
		if app.authStore.TrustedNetworksLocked() {
			effective := app.authStore.TrustedNetworksRaw()
			existingValue := ""
			if existing != nil {
				existingValue = existing.TrustedNetworks
			}
			if cfg.TrustedNetworks != effective && cfg.TrustedNetworks != existingValue {
				writeError(w, 403, "trusted_networks is locked by TRUSTED_NETWORKS env var and cannot be changed via the UI")
				return
			}
		}
		if app.authStore.TrustedProxiesLocked() {
			effective := app.authStore.TrustedProxiesRaw()
			existingValue := ""
			if existing != nil {
				existingValue = existing.TrustedProxies
			}
			if cfg.TrustedProxies != effective && cfg.TrustedProxies != existingValue {
				writeError(w, 403, "trusted_proxies is locked by TRUSTED_PROXIES env var and cannot be changed via the UI")
				return
			}
		}
	}

	if err := ValidateConfig(&cfg); err != nil {
		writeError(w, 400, fmt.Sprintf("Validation error: %v", err))
		return
	}

	if err := saveConfig(app.configPath, &cfg); err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to write config: %v", err))
		return
	}

	// Reload auth store in-process so Security-panel changes take effect
	// without restart. Silently skipped on parse errors — save already
	// succeeded and restart will apply the value correctly.
	if app.authStore != nil {
		newAuthCfg := app.authStore.Config()
		if cfg.Authentication != "" {
			newAuthCfg.Mode = auth.AuthMode(cfg.Authentication)
		}
		if cfg.AuthenticationRequired != "" {
			newAuthCfg.Requirement = auth.Requirement(cfg.AuthenticationRequired)
		}
		if cfg.SessionTTLDays > 0 {
			newAuthCfg.SessionTTL = time.Duration(cfg.SessionTTLDays) * 24 * time.Hour
		}
		if !newAuthCfg.TrustedNetworksLocked {
			if cfg.TrustedNetworks != "" {
				if nets, err := netsec.ParseTrustedNetworks(cfg.TrustedNetworks); err == nil {
					newAuthCfg.TrustedNetworks = nets
				}
			} else {
				newAuthCfg.TrustedNetworks = nil
			}
		}
		if !newAuthCfg.TrustedProxiesLocked {
			if cfg.TrustedProxies != "" {
				if ips, err := netsec.ParseTrustedProxies(cfg.TrustedProxies); err == nil {
					newAuthCfg.TrustedProxies = ips
				}
			} else {
				newAuthCfg.TrustedProxies = nil
			}
		}
		if err := app.authStore.UpdateConfig(newAuthCfg); err != nil {
			log.Printf("auth: UpdateConfig after save failed (will apply on restart): %v", err)
		}
	}

	// Config watcher in svc-traffic will pick up changes within 10s
	writeJSON(w, map[string]string{"status": "ok"})
}
