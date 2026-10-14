package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// newAutoSyncApp wires an App against a temp config directory, a fake
// qBittorrent, and a provider port of 31506, with auto-sync enabled.
func newAutoSyncApp(t *testing.T, f *fakeQBit, mapping PortMapping) *App {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wireguard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traffic.conf"), []byte("ENABLED=true\nINTERFACE=wg0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wireguard", "forwarded_port"), []byte("31506\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapping.AutoSyncForwardedPort = true
	uiCfg := map[string]any{"ports": []PortMapping{mapping}}
	data, _ := json.Marshal(uiCfg)
	uiPath := filepath.Join(dir, ".traffic-ui.json")
	if err := os.WriteFile(uiPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	saved := uiConfigPath
	uiConfigPath = uiPath
	t.Cleanup(func() { uiConfigPath = saved })
	return &App{
		configPath:    filepath.Join(dir, "traffic.conf"),
		iface:         "wg-does-not-exist",
		vpnConfigPath: filepath.Join(dir, "wireguard", "wg0.conf"),
		history:       NewHistoryStore(filepath.Join(dir, "history.json")),
	}
}

// The provider port must win over whatever qBittorrent is currently on,
// and a port that already agrees must not be rewritten on every tick.
func TestAutoSyncWritesOnlyWhenPortDiffers(t *testing.T) {
	f, mapping := newFakeQBit(t, true)
	f.setListenPort(12345)
	app := newAutoSyncApp(t, f, mapping)

	app.runQBitPortAutoSync()
	if _, port := f.state(); port != 31506 {
		t.Fatalf("listen_port = %d, want the provider port 31506", port)
	}
	if n := f.writeCount(); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}

	app.runQBitPortAutoSync()
	if n := f.writeCount(); n != 1 {
		t.Fatalf("writes = %d after a second pass, want no rewrite of a matching port", n)
	}
}

// Regression: the loop used to compare only against the port it had last
// pushed, recorded in history. A port changed anywhere else was then
// never corrected, so the dashboard reported a mismatch for ever.
func TestAutoSyncCorrectsDriftAfterItWasAlreadySynced(t *testing.T) {
	f, mapping := newFakeQBit(t, true)
	f.setListenPort(31506)
	app := newAutoSyncApp(t, f, mapping)
	app.history.SetAutoSyncedQBitPort(31506)

	// Someone changes the port in qBittorrent itself.
	f.setListenPort(9999)
	app.runQBitPortAutoSync()

	if _, port := f.state(); port != 31506 {
		t.Fatalf("listen_port = %d, want 31506 restored", port)
	}
	if n := f.writeCount(); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}
}

// With no instance opted in, nothing is written: enabling the sync stays
// the user's decision.
func TestAutoSyncDoesNothingWhenDisabled(t *testing.T) {
	f, mapping := newFakeQBit(t, true)
	f.setListenPort(12345)
	app := newAutoSyncApp(t, f, mapping)
	// Rewrite the UI config with the toggle off.
	mapping.AutoSyncForwardedPort = false
	data, _ := json.Marshal(map[string]any{"ports": []PortMapping{mapping}})
	if err := os.WriteFile(uiConfigPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	app.runQBitPortAutoSync()
	if _, port := f.state(); port != 12345 {
		t.Fatalf("listen_port = %d, want it left alone at 12345", port)
	}
	if n := f.writeCount(); n != 0 {
		t.Fatalf("writes = %d, want 0", n)
	}
}
