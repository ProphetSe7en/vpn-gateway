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
	"strings"
	"syscall"
	"time"
)

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
	mux.HandleFunc("GET /api/stats/daily", app.handleStatsDailyVolume)
	mux.HandleFunc("POST /api/stats/reset", app.handleStatsReset)

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

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "vpn-gateway"
	}
	// Strip .internal suffix if present
	if idx := strings.Index(hostname, "."); idx > 0 {
		hostname = hostname[:idx]
	}

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
		"hostname":        hostname,
	}

	writeJSON(w, status)
}

// GET /api/config — full parsed config
func (app *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to read config: %v", err))
		return
	}
	writeJSON(w, cfg)
}

// PUT /api/config — update config
func (app *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // 64KB limit
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, 400, fmt.Sprintf("Invalid JSON: %v", err))
		return
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
