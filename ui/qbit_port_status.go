package main

import (
	"context"
	"errors"
	"log"
	"net"
	"path/filepath"
	"sync"
	"time"
)

// QBitPortStatus is what the dashboard needs to tell the truth about one
// qBittorrent instance's listening port. Before this existed the UI showed
// the provider's forwarded port on its own, which looks confirmed but is
// only a number read out of hotio's forwarded_port file: it says nothing
// about whether qBittorrent actually listens there.
type QBitPortStatus struct {
	Name string `json:"name"`
	Port int    `json:"port"` // WebUI port, identifies the instance
	// ListenPort is the port qBittorrent reports for itself, or 0 when it
	// could not be read (see Problem).
	ListenPort int  `json:"listenPort"`
	AutoSync   bool `json:"autoSync"`
	// Problem is a short, user-facing reason why ListenPort is 0. Empty
	// when the port was read successfully.
	Problem string `json:"problem,omitempty"`
}

// qbitPortCache holds the last result of the listening-port check. The
// check talks to qBittorrent over HTTP, so it runs on a timer rather than
// inside the status handler: a slow or wedged qBit must not stall the
// dashboard.
type qbitPortCache struct {
	mu        sync.Mutex
	items     []QBitPortStatus
	checkedAt time.Time
}

func (c *qbitPortCache) get() ([]QBitPortStatus, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items, c.checkedAt
}

func (c *qbitPortCache) set(items []QBitPortStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = items
	c.checkedAt = time.Now()
}

// qbitPortCheckInterval is how often qBittorrent is asked for its current
// listening port. The value only changes when someone edits it in qBit or
// when our own auto-sync pushes a new one, so a minute is plenty and keeps
// the API traffic negligible.
const qbitPortCheckInterval = 60 * time.Second

// refreshQBitPortStatus asks every configured qBittorrent instance which
// port it listens on, and caches the answers for the dashboard.
//
// It only runs when the provider publishes a dynamic forwarded port (PIA,
// Proton). Without one there is nothing to compare against: users on
// static ports (TorGuard and friends) set the port themselves in both
// places and must not be nagged about it.
func (app *App) refreshQBitPortStatus() {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		log.Printf("qbit-port-check: load config: %v", err)
		return
	}
	if readDynamicForwardedPort(filepath.Dir(app.vpnConfigPath)) == 0 {
		app.qbitPorts.set(nil)
		return
	}
	qbitP, _ := pollerFor("qbittorrent").(*qBitPoller)
	if qbitP == nil {
		// Nothing can be checked, so drop what we had rather than leaving
		// the dashboard with values that are no longer being refreshed.
		app.qbitPorts.set(nil)
		return
	}
	var items []QBitPortStatus
	for _, pm := range cfg.Ports {
		if pm.Type != "" && pm.Type != "qbittorrent" {
			continue
		}
		item := QBitPortStatus{Name: pm.Name, Port: pm.Port, AutoSync: pm.AutoSyncForwardedPort}
		// Always ask first. Blank credentials are legitimate on instances
		// with LocalHostAuth=false, which answer without any login, so the
		// credentials hint is only correct when qBit itself demands one.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		port, err := qbitP.ListeningPort(ctx, pm)
		cancel()
		switch {
		case err == nil:
			item.ListenPort = port
		case errors.Is(err, errQBitLoginRequired):
			item.Problem = "add this instance's qBittorrent Username and Password so the port can be checked"
		case isConnectionError(err):
			// Usually just a qBittorrent that has not finished starting, or
			// one that is down. Saying "could not read" there sounds like a
			// fault in the port check itself.
			item.Problem = "qBittorrent is not answering on its web UI port"
		default:
			item.Problem = "could not read the listening port from qBittorrent"
			log.Printf("qbit-port-check: %s: %v", sanitizeLogField(pm.Name), err)
		}
		items = append(items, item)
	}
	app.qbitPorts.set(items)
}

// logQBitPortState writes one line describing what the port sync is doing,
// so "is this working?" can be answered from the log. The auto-sync loop
// itself is silent unless it pushes a port or hits an error, which left
// users unable to tell a working setup from one that was never switched on.
//
// It logs once, on the first check where the provider has actually
// published a port. On PIA and Proton that file appears a while after
// start (tunnel handshake, then the port request), so logging only at
// startup would usually have said nothing at all.
func (app *App) logQBitPortState() {
	if app.qbitStateLogged.Load() {
		return
	}
	dynamic := readDynamicForwardedPort(filepath.Dir(app.vpnConfigPath))
	if dynamic == 0 {
		return
	}
	items, _ := app.qbitPorts.get()
	if len(items) == 0 {
		app.qbitStateLogged.Store(true)
		log.Printf("qbit-port-sync: provider port is %d, no qBittorrent instance is configured", dynamic)
		return
	}
	// A failed first check is usually just qBittorrent still starting, so
	// the "what is the port sync doing" line waits for an answer we can
	// actually act on. The problem itself is still reported once, so a
	// permanent failure is not silent either.
	unresolved := false
	for _, it := range items {
		if it.Problem != "" {
			unresolved = true
		}
	}
	if unresolved {
		if app.qbitProblemLogged.Swap(true) {
			return
		}
	} else {
		app.qbitStateLogged.Store(true)
	}
	for _, it := range items {
		name := sanitizeLogField(it.Name)
		switch {
		case it.Problem != "":
			log.Printf("qbit-port-sync: %s: provider port is %d, cannot check qBittorrent's port (%s)", name, dynamic, it.Problem)
		case it.ListenPort == dynamic:
			log.Printf("qbit-port-sync: %s: listening on %d, matches the provider port", name, it.ListenPort)
		case it.AutoSync:
			log.Printf("qbit-port-sync: %s: listening on %d, provider port is %d, auto-sync is on and will update it", name, it.ListenPort, dynamic)
		default:
			log.Printf("qbit-port-sync: %s: listening on %d, provider port is %d, auto-sync is off", name, it.ListenPort, dynamic)
		}
	}
}

// isConnectionError reports whether the failure was in reaching
// qBittorrent at all, rather than in what it answered.
func isConnectionError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}
