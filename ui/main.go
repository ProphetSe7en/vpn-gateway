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

//go:embed static
var staticFiles embed.FS

type App struct {
	configPath string
}

func main() {
	port := os.Getenv("UI_PORT")
	if port == "" {
		port = "8090"
	}

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/traffic.conf"
	}

	app := &App{configPath: configPath}

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/status", app.handleStatus)
	mux.HandleFunc("GET /api/config", app.handleGetConfig)
	mux.HandleFunc("PUT /api/config", app.handlePutConfig)

	// Static files
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
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

	status := map[string]any{
		"enabled":         cfg.Enabled,
		"scheduleEnabled": cfg.ScheduleEnabled,
		"defaultDown":     cfg.DefaultDown,
		"defaultUp":       cfg.DefaultUp,
		"burstMs":         cfg.BurstMs,
		"rules":           cfg.Rules,
		"activeUp":        upRule,
		"activeDown":      downRule,
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
