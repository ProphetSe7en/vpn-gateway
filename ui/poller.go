package main

import (
	"context"
)

// ServiceStats is the normalized response returned by any ServicePoller.
// All fields are in raw bytes / bytes-per-second so the counter logic in
// stats.go can apply the same restart-detection and delta math regardless
// of which backend service produced the numbers.
type ServiceStats struct {
	SessionRx int64 // Cumulative bytes received this session (download)
	SessionTx int64 // Cumulative bytes sent this session (upload)
	LiveRx    int64 // Current speed bytes/sec (download)
	LiveTx    int64 // Current speed bytes/sec (upload)
	Active    int   // Optional: active items (downloads / streams / clients)
}

// ServicePoller is the interface every supported service must implement.
// Poll is called once per collection tick for each PortMapping. The context
// carries the per-poll timeout. Implementations MUST NOT hold any global
// lock during HTTP work — the caller in stats.go drains results before
// re-acquiring the TrafficCollector mutex.
type ServicePoller interface {
	Poll(ctx context.Context, mapping PortMapping) (ServiceStats, error)
	Type() string
}

// ServiceVerifier is an optional extension for pollers that need to be
// tested without mutating any internal state. The Test endpoint uses this
// instead of Poll for stateful pollers like Dispatcharr — calling Poll for
// a connectivity test would otherwise leak the test cycle's bytes into the
// real session counter, and could clobber a running poller's state for the
// same port. Pollers that do not implement this interface fall back to Poll.
type ServiceVerifier interface {
	ServicePoller
	Verify(ctx context.Context, mapping PortMapping) error
}

// ServiceDetailer is an optional extension implemented by pollers that
// can expose a richer detail view beyond the aggregate ServiceStats.
// Dispatcharr uses this for the Active Streams panel on the Traffic tab
// (per-channel name, client count, bitrate, connected clients).
//
// The returned value is an opaque `any` — the frontend renders it based
// on the poller's Type(). Pollers that do not implement this interface
// are silently skipped when the frontend asks for details.
//
// Details is only called when the user has enabled ShowDetails on the
// PortMapping config. Pollers must NOT expose credentials, internal
// URLs, or any field that could leak provider authentication data.
type ServiceDetailer interface {
	ServicePoller
	Details(ctx context.Context, mapping PortMapping) (any, error)
}

// servicePollers is the registry of known poller types. The empty-string
// entry is deliberate — it gives backward compatibility for existing configs
// whose PortMapping entries have no Type field (treated as qBittorrent).
//
// Additional pollers are registered by appending to this map in their own
// file's init() function, e.g. poller_dispatcharr.go and poller_sab.go.
var servicePollers = map[string]ServicePoller{}

// registerPoller is called from each poller implementation's init() to
// register itself under one or more type keys. The empty string key is
// reserved for qBittorrent as the backward-compatible default.
func registerPoller(p ServicePoller, keys ...string) {
	for _, k := range keys {
		servicePollers[k] = p
	}
}

// pollerFor returns the poller for the given mapping type, falling back to
// the qBittorrent default when the type is empty or unknown. Callers should
// treat an unknown type as qBittorrent rather than erroring — this keeps
// the config file forward-compatible if a user downgrades vpn-gateway.
func pollerFor(serviceType string) ServicePoller {
	if p, ok := servicePollers[serviceType]; ok {
		return p
	}
	return servicePollers[""]
}
