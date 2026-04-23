package main

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// IPPortHistoryEntry tracks a single observable value with its prior
// value and when the most recent change was detected. Used for exit IP,
// tunnel IP, and the dynamic forwarded port — anything that can change
// at runtime and where the user benefits from "this just changed"
// awareness in the dashboard.
//
// Static port redirects from VPN_PORT_REDIRECTS are admin-managed
// constants and intentionally NOT tracked here — there's no meaningful
// "changed N hours ago" for a value the user explicitly set in their
// container template.
type IPPortHistoryEntry struct {
	Value     string    `json:"value"`              // current value (string for unified marshalling)
	Previous  string    `json:"previous,omitempty"` // last different value (empty before first change)
	ChangedAt time.Time `json:"changedAt,omitempty"`
}

type IPPortHistory struct {
	ExitIP               IPPortHistoryEntry `json:"exitIp"`
	TunnelIP             IPPortHistoryEntry `json:"tunnelIp"`
	ForwardedPortDynamic IPPortHistoryEntry `json:"forwardedPortDynamic"`

	// AutoSyncedQBitPort is the last port value vpn-gateway pushed into
	// qBit via setPreferences for the auto-sync feature. Persisted so a
	// container restart with the same forwarded port doesn't re-issue
	// the API call. 0 = never synced. Cleared when the user disables
	// the feature on all ports.
	AutoSyncedQBitPort int `json:"autoSyncedQBitPort,omitempty"`
}

// HistoryStore persists the last-observed state to disk so "changed N
// minutes ago" stays truthful across container restarts (otherwise every
// restart would falsely flag the same on-disk value as just-changed).
//
// Writes are atomic + serialised behind mu — concurrent /api/status
// callers cannot interleave read-modify-write on the file.
type HistoryStore struct {
	path  string
	mu    sync.Mutex
	state IPPortHistory
	once  sync.Once
}

func NewHistoryStore(path string) *HistoryStore {
	return &HistoryStore{path: path}
}

// loadOnce reads the history file the first time the store is touched.
// Errors are silent: a missing or corrupt file means we start fresh,
// which at worst flags every value as new on first boot — acceptable
// degradation for a UI-only feature.
func (h *HistoryStore) loadOnce() {
	h.once.Do(func() {
		data, err := os.ReadFile(h.path)
		if err != nil {
			return
		}
		_ = json.Unmarshal(data, &h.state)
	})
}

// Update reconciles current observations against persisted state and
// returns the resulting history snapshot. Empty current values mean
// "not observable right now" (interface down, file missing) — those
// fields are NOT updated, so the user keeps seeing the last-known value
// instead of having it disappear during a brief connection drop.
func (h *HistoryStore) Update(exitIP, tunnelIP string, forwardedDynamic int) IPPortHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadOnce()

	now := time.Now().UTC()
	changed := false
	if exitIP != "" {
		if e, c := updateEntry(h.state.ExitIP, exitIP, now); c {
			h.state.ExitIP, changed = e, true
		}
	}
	if tunnelIP != "" {
		if e, c := updateEntry(h.state.TunnelIP, tunnelIP, now); c {
			h.state.TunnelIP, changed = e, true
		}
	}
	if forwardedDynamic > 0 {
		fwd := strconv.Itoa(forwardedDynamic)
		if e, c := updateEntry(h.state.ForwardedPortDynamic, fwd, now); c {
			h.state.ForwardedPortDynamic, changed = e, true
		}
	}

	if changed {
		if err := h.persist(); err != nil {
			log.Printf("history: persist failed: %v", err)
		}
	}
	return h.state
}

// AutoSyncedQBitPort returns the last value the auto-sync goroutine
// pushed into qBit. 0 means never synced.
func (h *HistoryStore) AutoSyncedQBitPort() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadOnce()
	return h.state.AutoSyncedQBitPort
}

// SetAutoSyncedQBitPort records that the given port was pushed to qBit
// successfully and persists the state. Caller invokes this only on
// successful API call.
func (h *HistoryStore) SetAutoSyncedQBitPort(port int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadOnce()
	if h.state.AutoSyncedQBitPort == port {
		return
	}
	h.state.AutoSyncedQBitPort = port
	if err := h.persist(); err != nil {
		log.Printf("history: persist autoSyncedQBitPort failed: %v", err)
	}
}

// updateEntry returns the new entry + whether anything actually changed.
// First observation (prev.Value == "") seeds the entry without recording
// a change — the value didn't transition, we just learned about it.
func updateEntry(prev IPPortHistoryEntry, current string, now time.Time) (IPPortHistoryEntry, bool) {
	if prev.Value == "" {
		return IPPortHistoryEntry{Value: current}, true
	}
	if prev.Value == current {
		return prev, false
	}
	return IPPortHistoryEntry{
		Value:     current,
		Previous:  prev.Value,
		ChangedAt: now,
	}, true
}

// persist atomically writes the state. 0600 because exit IP / tunnel IP
// reveal which VPN server / region you're connected to — should not be
// readable by other containers via shared /config dir conventions.
func (h *HistoryStore) persist() error {
	data, err := json.MarshalIndent(h.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(h.path, data, 0600)
}
