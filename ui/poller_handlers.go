package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// handleServiceDetails returns the service-specific detail payload for
// the configured port, if the poller implements ServiceDetailer.
//
//	GET /api/service-details?port=9191
//
// Response on success: 200 with JSON body
//
//	{
//	  "type": "dispatcharr",
//	  "port": 9191,
//	  "name": "Dispatcharr",
//	  "details": <poller-specific payload>
//	}
//
// If the port is not configured: 404.
// If the poller does not support details: 204 (no content).
// If fetching details fails: 502 with error message.
func (app *App) handleServiceDetails(w http.ResponseWriter, r *http.Request) {
	portStr := r.URL.Query().Get("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid port")
		return
	}

	cfg, err := loadConfig(app.configPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read config")
		return
	}
	var mapping *PortMapping
	for i, pm := range cfg.Ports {
		if pm.Port == port {
			mapping = &cfg.Ports[i]
			break
		}
	}
	if mapping == nil {
		writeError(w, http.StatusNotFound, "port not configured")
		return
	}

	poller := pollerFor(mapping.Type)
	if poller == nil {
		writeError(w, http.StatusNotFound, "no poller for type")
		return
	}
	detailer, ok := poller.(ServiceDetailer)
	if !ok {
		// Poller exists but has no detail view — not an error, just
		// nothing to show.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	payload, err := detailer.Details(ctx, *mapping)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":    poller.Type(),
		"port":    mapping.Port,
		"name":    mapping.Name,
		"details": payload,
	})
}

// handleTestPort exercises a single Poll against the provided mapping,
// returning success + a brief stats summary or the error message. The
// config is NOT modified — this endpoint is for the "Test" button in
// the Service Monitoring UI so the operator can verify credentials
// before saving.
//
//	POST /api/test-port
//	{
//	  "port": 9191,
//	  "type": "dispatcharr",
//	  "username": "admin",
//	  "password": "..."
//	}
//
// Response: 200 with {"ok": true, "stats": {...}} or
// 400/502 with {"ok": false, "error": "..."}.
func (app *App) handleTestPort(w http.ResponseWriter, r *http.Request) {
	// Cap the request body. PortMapping is tiny (under 1 KB even with long
	// credentials) — 8 KB leaves comfortable headroom while preventing an
	// attacker on the local network from POSTing an unbounded body and
	// forcing vpn-gateway to allocate arbitrary memory while decoding.
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var mapping PortMapping
	if err := json.NewDecoder(r.Body).Decode(&mapping); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if mapping.Port <= 0 || mapping.Port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid port")
		return
	}
	poller := pollerFor(mapping.Type)
	if poller == nil {
		writeError(w, http.StatusBadRequest, "unknown service type")
		return
	}

	// Credential mask resolution: /api/config masks Password/APIKey as
	// credentialMask on GET, so when the user clicks "Test" without
	// re-entering secrets, the UI sends the sentinel back. Without this
	// merge, Verify/Poll would authenticate with the literal "********"
	// and fail even when stored credentials are correct. Mirror
	// handlePutConfig's merge: look up the port by number in on-disk
	// config and swap the sentinel for the real value.
	if mapping.Password == credentialMask || mapping.APIKey == credentialMask {
		existing, err := loadConfig(app.configPath)
		if err == nil && existing != nil {
			for _, pm := range existing.Ports {
				if pm.Port != mapping.Port {
					continue
				}
				if mapping.Password == credentialMask {
					mapping.Password = pm.Password
				}
				if mapping.APIKey == credentialMask {
					mapping.APIKey = pm.APIKey
				}
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")

	// Prefer the non-stateful Verify path when the poller supports it —
	// avoids leaking test cycle bytes into the live session counter and
	// avoids clobbering an active poller's per-port state. Stateless
	// pollers (like qBit) fall back to Poll, which is idempotent for them.
	if v, ok := poller.(ServiceVerifier); ok {
		if err := v.Verify(ctx, mapping); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"type":  poller.Type(),
				"error": err.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"type": poller.Type(),
		})
		return
	}

	stats, err := poller.Poll(ctx, mapping)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"type":  poller.Type(),
			"error": err.Error(),
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":   true,
		"type": poller.Type(),
		"stats": map[string]any{
			"sessionRx": stats.SessionRx,
			"sessionTx": stats.SessionTx,
			"liveRx":    stats.LiveRx,
			"liveTx":    stats.LiveTx,
			"active":    stats.Active,
		},
	})
}
