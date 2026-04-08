package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Version is set at build time via ldflags
var version = "dev"

//go:embed static
var staticFiles embed.FS

type App struct {
	configPath string
	traffic    *TrafficCollector
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
	go traffic.Run(ctx)

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

	// Static files
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
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

// --- API Handlers ---

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
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
		"version":         version,
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
// SECURITY: vpn-gateway's web UI has no auth layer of its own — anyone who
// can reach the listening port can read this endpoint. Returning credentials
// in plain text would leak the Dispatcharr admin password and SAB API key
// to anything on the same network. We mask them with credentialMask on read
// and merge the originals back in handlePutConfig if the frontend sends the
// mask value unchanged.
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
// Credential merge: if a port's Password / APIKey field comes in as the
// credentialMask sentinel, we look up the matching port in the on-disk
// config and restore the real value before saving. This is what allows
// the masked GET response to round-trip safely — the frontend can edit
// other fields and save without ever seeing the real credentials.
func (app *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // 64KB limit
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, 400, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Restore masked credentials from the on-disk config before validating.
	// We only attempt the lookup if at least one mask is present, to keep
	// the common-case write (no credentials at all, or all freshly typed)
	// from doing an extra disk read.
	needsMerge := false
	for _, pm := range cfg.Ports {
		if pm.Password == credentialMask || pm.APIKey == credentialMask {
			needsMerge = true
			break
		}
	}
	if needsMerge {
		existing, err := loadConfig(app.configPath)
		if err == nil {
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
	}

	if err := ValidateConfig(&cfg); err != nil {
		writeError(w, 400, fmt.Sprintf("Validation error: %v", err))
		return
	}

	if err := saveConfig(app.configPath, &cfg); err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to write config: %v", err))
		return
	}

	// Config watcher in svc-traffic will pick up changes within 10s
	writeJSON(w, map[string]string{"status": "ok"})
}
