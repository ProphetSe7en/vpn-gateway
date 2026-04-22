package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	configPath string
	traffic    *TrafficCollector
	authStore  *auth.Store
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

	// Start traffic collector
	traffic := NewTrafficCollector(iface, configPath)
	ctx, cancel := context.WithCancel(context.Background())
	safeGo("traffic-collector", func() { traffic.Run(ctx) })

	app := &App{configPath: configPath, traffic: traffic}

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
	}

	writeJSON(w, status)
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
	if existing != nil {
		oldByPort := make(map[int]PortMapping, len(existing.Ports))
		for _, pm := range existing.Ports {
			oldByPort[pm.Port] = pm
		}
		for i := range cfg.Ports {
			old, ok := oldByPort[cfg.Ports[i].Port]
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
