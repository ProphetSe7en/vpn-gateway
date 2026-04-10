package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// dispatcharrPoller implements ServicePoller for Dispatcharr's stream
// status API. Dispatcharr authenticates with JWT obtained via username/
// password POST to /api/accounts/token/ — there is no API key mechanism
// in v0.x. Tokens are cached per port (since multiple Dispatcharr
// instances might be configured in theory) and refreshed on 401.
//
// SECURITY NOTE — the /proxy/ts/status response contains an `url` field
// that leaks the upstream IPTV provider's username and password in
// plaintext. This poller MUST NOT log, emit, persist, or return that
// field anywhere. The dispatcharrChannel struct deliberately omits it.
// See multi-service-monitoring-plan.md § "Security warning".
type dispatcharrPoller struct {
	mu            sync.Mutex
	tokens        map[int]*dispatcharrToken       // keyed by mapping.Port
	state         map[int]*dispatcharrPortState   // keyed by mapping.Port
	cached        map[int]ServiceStats            // last successful Poll result per port
	ticks         map[int]uint64                  // per-port poll counter for throttling
	cachedDetails map[int]cachedDispatcharrDetails // last Details result per port
}

// dispatcharrPortState is per-configured-port state that survives across
// polls but NOT across process restarts. We need it because Dispatcharr's
// /proxy/ts/status endpoint returns per-channel `total_bytes` counters,
// not a flat session total — so naive aggregation (sum across channels
// each poll) over-counts when channels start or stop concurrently.
//
// The fix: we maintain our own cumulative `sessionBytes` by computing
// per-channel deltas ourselves and adding them up. A channel that
// restarts (new total_bytes < previous) is treated as having started
// fresh, so we add its new value rather than the delta.
type dispatcharrPortState struct {
	sessionBytes int64                   // cumulative bytes across all poll cycles in this process
	channelBytes map[string]int64        // last-seen total_bytes per channel_id
	rateSamples  []dispatcharrRateSample // sliding window of (timestamp, sessionBytes) for smoothed live rate
	lastLiveRx   int64                   // last smoothed live rx rate (bytes/sec)
	lastPollAt   time.Time               // wall-clock of previous successful Poll — used for gap detection
}

// dispatcharrGapThreshold mirrors sabGapThreshold: if more than this elapses
// between successful polls (vpn-gateway down, Dispatcharr unreachable, or
// Dispatcharr moved out of our network namespace and back), we treat the
// next poll as a fresh baseline rather than trusting the channelBytes map.
// 60 s gives ~4 missed real polls at the throttled ~15 s cadence.
const dispatcharrGapThreshold = 60 * time.Second

// dispatcharrRateSample is one (timestamp, cumulative bytes) data point in the
// smoothing window. We keep ~30 s worth (must exceed the effective poll
// interval of ~15 s) so the live rate is robust to Dispatcharr's chunked
// /proxy/ts/status updates — its total_bytes counter often sits still for
// several polls then jumps, so a poll-to-poll delta would oscillate between
// 0 and inflated values.
type dispatcharrRateSample struct {
	at           time.Time
	sessionBytes int64
}

// dispatcharrRateWindow is how far back we look when computing the smoothed
// live rate. Must be larger than the effective poll interval (currently ~15 s
// due to dispatcharrPollInterval=5 at 3 s sample rate) so we always have at
// least 2 samples in the window. 30 s keeps 2-3 data points.
const dispatcharrRateWindow = 30 * time.Second

// dispatcharrPollInterval controls how often we actually hit the Dispatcharr
// API. Every Nth call to Poll() makes a real request; the others return the
// cached result. This avoids overwhelming Dispatcharr's API while keeping
// the stats/graph pipelines fed every tick.
const dispatcharrPollInterval = 5 // ~15s at 3s sample interval

// dispatcharrDetailsTTL controls how long a cached Details() result is
// reused before making a new API call. Frontend polls every 5 s, so a
// 30 s TTL means we hit the API roughly every 6th frontend poll.
const dispatcharrDetailsTTL = 30 * time.Second

type cachedDispatcharrDetails struct {
	data any
	at   time.Time
}

// dispatcharrToken caches a JWT pair for a single Dispatcharr instance.
// We don't rely on the JWT's exp claim — we just refresh on 401.
type dispatcharrToken struct {
	access  string
	refresh string
}

// dispatcharrLoginReq / dispatcharrLoginResp are the JWT login endpoint
// request and response shapes. Verified against a live instance 2026-04-08.
type dispatcharrLoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type dispatcharrLoginResp struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
}

// dispatcharrRefreshReq / dispatcharrRefreshResp mirror the token refresh
// endpoint. A 401 on refresh means we need a full re-login.
type dispatcharrRefreshReq struct {
	Refresh string `json:"refresh"`
}

type dispatcharrRefreshResp struct {
	Access string `json:"access"`
}

// dispatcharrStatus is the /proxy/ts/status response. We deliberately do
// NOT decode the `url`, `owner`, `stream_profile`, or `m3u_profile_*`
// fields — they either contain credentials or serve no purpose for
// traffic stats, and leaving them out of the struct means Go's json
// decoder silently drops them so they can never accidentally be logged
// or emitted.
type dispatcharrStatus struct {
	Channels []dispatcharrChannel `json:"channels"`
	Count    int                  `json:"count"`
}

type dispatcharrChannel struct {
	ChannelID      string              `json:"channel_id"`
	State          string              `json:"state"`
	StreamName     string              `json:"stream_name"`
	ClientCount    int                 `json:"client_count"`
	TotalBytes     int64               `json:"total_bytes"`
	AvgBitrateKbps float64             `json:"avg_bitrate_kbps"`
	Uptime         float64             `json:"uptime"`
	VideoCodec     string              `json:"video_codec"`
	Resolution     string              `json:"resolution"`
	AudioCodec     string              `json:"audio_codec"`
	AudioChannels  string              `json:"audio_channels"`
	Clients        []dispatcharrClient `json:"clients"`
}

type dispatcharrClient struct {
	ClientID       string  `json:"client_id"`
	UserAgent      string  `json:"user_agent"`
	IPAddress      string  `json:"ip_address"`
	ConnectedSince float64 `json:"connected_since"`
}

// Type returns the canonical type key for Dispatcharr.
func (d *dispatcharrPoller) Type() string { return "dispatcharr" }

// getToken returns a cached access token for this mapping, logging in if
// we have no cached token yet. This is called under the poller mutex.
func (d *dispatcharrPoller) getToken(ctx context.Context, mapping PortMapping) (string, error) {
	if d.tokens == nil {
		d.tokens = make(map[int]*dispatcharrToken)
	}
	if t, ok := d.tokens[mapping.Port]; ok && t.access != "" {
		return t.access, nil
	}
	return d.login(ctx, mapping)
}

// login performs a fresh JWT login and caches the resulting token pair.
// Must be called with d.mu held.
func (d *dispatcharrPoller) login(ctx context.Context, mapping PortMapping) (string, error) {
	if mapping.Username == "" || mapping.Password == "" {
		return "", fmt.Errorf("dispatcharr: missing username/password in config")
	}
	body, _ := json.Marshal(dispatcharrLoginReq{
		Username: mapping.Username,
		Password: mapping.Password,
	})
	url := dispatcharrURL(mapping, "/api/accounts/token/")
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dispatcharr: login status %d", resp.StatusCode)
	}
	var lr dispatcharrLoginResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return "", err
	}
	if lr.Access == "" {
		return "", fmt.Errorf("dispatcharr: login returned empty access token")
	}
	d.tokens[mapping.Port] = &dispatcharrToken{access: lr.Access, refresh: lr.Refresh}
	return lr.Access, nil
}

// refreshAccess tries to use the cached refresh token to get a new access
// token. If the refresh token is itself expired, we force a full re-login.
// Must be called with d.mu held.
func (d *dispatcharrPoller) refreshAccess(ctx context.Context, mapping PortMapping) (string, error) {
	t, ok := d.tokens[mapping.Port]
	if !ok || t.refresh == "" {
		return d.login(ctx, mapping)
	}
	body, _ := json.Marshal(dispatcharrRefreshReq{Refresh: t.refresh})
	url := dispatcharrURL(mapping, "/api/accounts/token/refresh/")
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Refresh token itself expired — drop cached state and re-login.
		delete(d.tokens, mapping.Port)
		return d.login(ctx, mapping)
	}
	var rr dispatcharrRefreshResp
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return "", err
	}
	t.access = rr.Access
	return rr.Access, nil
}

// Poll fetches the Dispatcharr status endpoint and returns aggregate stats.
// Token lifecycle: try cached access, refresh on 401, relogin on refresh
// failure. The entire chain happens under the poller mutex so concurrent
// polls for the same Dispatcharr instance don't double-login.
//
// Session bytes are computed from per-channel deltas (see dispatcharrPortState)
// rather than naive sum(total_bytes), because Dispatcharr tracks per-channel
// counters and a naive sum double-counts when channels start/stop concurrently.
func (d *dispatcharrPoller) Poll(ctx context.Context, mapping PortMapping) (ServiceStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Throttle: only hit the API every Nth call per port. On skipped ticks,
	// return the cached result so graphs and stats stay populated.
	if d.ticks == nil {
		d.ticks = make(map[int]uint64)
	}
	d.ticks[mapping.Port]++
	if d.ticks[mapping.Port]%dispatcharrPollInterval != 0 {
		if d.cached != nil {
			if s, ok := d.cached[mapping.Port]; ok {
				return s, nil
			}
		}
		// No cached result yet — fall through to do a real poll
	}

	status, err := d.fetchStatus(ctx, mapping)
	if err != nil {
		return ServiceStats{}, err
	}

	if d.state == nil {
		d.state = make(map[int]*dispatcharrPortState)
	}
	ps, ok := d.state[mapping.Port]
	if !ok {
		ps = &dispatcharrPortState{channelBytes: make(map[string]int64)}
		d.state[mapping.Port] = ps
	}

	// Gap detection: if too long elapsed since the last successful poll,
	// rebaseline channelBytes from the current snapshot rather than trusting
	// the stale per-channel deltas. This handles vpn-gateway being offline,
	// Dispatcharr being unreachable, or it being moved between networks.
	// We populate channelBytes with the current snapshot so the *next* poll
	// computes deltas normally (prev == current → delta == 0).
	if !ps.lastPollAt.IsZero() && time.Since(ps.lastPollAt) > dispatcharrGapThreshold {
		ps.channelBytes = make(map[string]int64)
		for _, ch := range status.Channels {
			ps.channelBytes[ch.ChannelID] = ch.TotalBytes
		}
		ps.rateSamples = nil // sliding window is from before the gap, drop it
		ps.lastLiveRx = 0
		ps.lastPollAt = time.Now()
		active := 0
		for _, ch := range status.Channels {
			active += ch.ClientCount
		}
		result := ServiceStats{
			SessionRx: ps.sessionBytes,
			SessionTx: 0,
			LiveRx:    0,
			LiveTx:    0,
			Active:    active,
		}
		if d.cached == nil {
			d.cached = make(map[int]ServiceStats)
		}
		d.cached[mapping.Port] = result
		return result, nil
	}

	// Accumulate per-channel deltas into the session counter.
	// Sum the cycle delta separately so LiveRx can be derived from real
	// bytes/elapsed instead of avg_bitrate_kbps. Using avg_bitrate_kbps
	// (a smoothed source bitrate) as LiveRx made the Stats tab 1h/6h/24h
	// charts (which integrate LiveRx × time) drift away from the per-service
	// total (which accumulates real byte deltas). Now both derive from the
	// same numbers and stay consistent.
	var cycleDelta int64
	active := 0
	for _, ch := range status.Channels {
		prev, seen := ps.channelBytes[ch.ChannelID]
		var delta int64
		if !seen || ch.TotalBytes < prev {
			// First time seen OR counter went backwards (channel restarted
			// inside Dispatcharr — e.g. client disconnected and reconnected).
			// Treat the new value as entirely fresh bytes.
			delta = ch.TotalBytes
		} else {
			delta = ch.TotalBytes - prev
		}
		cycleDelta += delta
		ps.channelBytes[ch.ChannelID] = ch.TotalBytes
		active += ch.ClientCount
	}
	ps.sessionBytes += cycleDelta

	// Append a sample and trim anything older than the smoothing window.
	// Live rate = (newest - oldest) / elapsed, which absorbs the chunked
	// counter updates Dispatcharr emits.
	now := time.Now()
	ps.rateSamples = append(ps.rateSamples, dispatcharrRateSample{at: now, sessionBytes: ps.sessionBytes})
	cutoff := now.Add(-dispatcharrRateWindow)
	trim := 0
	for trim < len(ps.rateSamples)-1 && ps.rateSamples[trim].at.Before(cutoff) {
		trim++
	}
	if trim > 0 {
		ps.rateSamples = ps.rateSamples[trim:]
	}
	if len(ps.rateSamples) >= 2 {
		oldest := ps.rateSamples[0]
		elapsed := now.Sub(oldest.at).Seconds()
		if elapsed > 0 {
			ps.lastLiveRx = int64(float64(ps.sessionBytes-oldest.sessionBytes) / elapsed)
		}
	}
	ps.lastPollAt = now
	// Note: channels that disappear from the response stay in channelBytes.
	// If they come back later with total_bytes starting over, the "counter
	// went backwards" branch above catches it and counts the new bytes from
	// zero. The map grows O(unique channels seen during process lifetime),
	// which is bounded in practice and trimmed on vpn-gateway restart.

	result := ServiceStats{
		SessionRx: ps.sessionBytes,
		SessionTx: 0, // Dispatcharr is download-only from vpn-gateway's perspective
		LiveRx:    ps.lastLiveRx,
		LiveTx:    0,
		Active:    active,
	}
	if d.cached == nil {
		d.cached = make(map[int]ServiceStats)
	}
	d.cached[mapping.Port] = result
	return result, nil
}

// fetchStatus makes the actual GET request, handling the token refresh
// cascade transparently. Must be called with d.mu held.
func (d *dispatcharrPoller) fetchStatus(ctx context.Context, mapping PortMapping) (*dispatcharrStatus, error) {
	token, err := d.getToken(ctx, mapping)
	if err != nil {
		return nil, err
	}
	status, code, err := d.getStatusWith(ctx, mapping, token)
	if err == nil && code == http.StatusOK {
		return status, nil
	}
	if code == http.StatusUnauthorized {
		// Access token expired — try refresh, then retry the request.
		newToken, refreshErr := d.refreshAccess(ctx, mapping)
		if refreshErr != nil {
			return nil, refreshErr
		}
		status2, code2, err2 := d.getStatusWith(ctx, mapping, newToken)
		if err2 != nil {
			return nil, err2
		}
		if code2 != http.StatusOK {
			return nil, fmt.Errorf("dispatcharr: status %d after token refresh", code2)
		}
		return status2, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("dispatcharr: unexpected status %d", code)
}

// getStatusWith makes a single GET /proxy/ts/status request using the
// provided bearer token. Returns decoded status, HTTP status code, and
// any transport-level error. A 401 response is not a Go error — the
// caller uses the status code to decide whether to refresh.
func (d *dispatcharrPoller) getStatusWith(ctx context.Context, mapping PortMapping, token string) (*dispatcharrStatus, int, error) {
	url := dispatcharrURL(mapping, "/proxy/ts/status")
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var status dispatcharrStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, resp.StatusCode, err
	}
	return &status, resp.StatusCode, nil
}

// dispatcharrURL constructs the full URL for a Dispatcharr endpoint.
// All requests go to localhost:<port><path> since vpn-gateway shares a
// network namespace with the services it monitors. Cross-host monitoring
// is not in scope — users who want it can expose Dispatcharr via a
// reverse proxy on the same namespace.
func dispatcharrURL(mapping PortMapping, path string) string {
	return fmt.Sprintf("http://localhost:%d%s", mapping.Port, path)
}

// --- ServiceDetailer implementation (Phase 3b: Active Streams panel) ---

// dispatcharrStreamDetail is the safe, credential-free projection of a
// dispatcharr channel exposed to the frontend. NEVER add the `url` field.
type dispatcharrStreamDetail struct {
	ChannelID      string                  `json:"channelId"`
	StreamName     string                  `json:"streamName"`
	ClientCount    int                     `json:"clientCount"`
	AvgBitrateKbps float64                 `json:"avgBitrateKbps"`
	UptimeSeconds  float64                 `json:"uptimeSeconds"`
	VideoCodec     string                  `json:"videoCodec,omitempty"`
	Resolution     string                  `json:"resolution,omitempty"`
	AudioCodec     string                  `json:"audioCodec,omitempty"`
	AudioChannels  string                  `json:"audioChannels,omitempty"`
	Clients        []dispatcharrClientInfo `json:"clients,omitempty"`
}

type dispatcharrClientInfo struct {
	ClientID       string  `json:"clientId"`
	IPAddress      string  `json:"ipAddress"`
	UserAgent      string  `json:"userAgent"`
	ConnectedSince float64 `json:"connectedSince"`
}

// Details implements ServiceDetailer — returns the list of currently
// active streams as a generic serialisable payload. The payload is
// shaped for the Dispatcharr-specific Active Streams UI panel but
// transported through a generic interface so future services (SAB's
// active job list, qBit's active torrents) can reuse the same plumbing.
func (d *dispatcharrPoller) Details(ctx context.Context, mapping PortMapping) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Return cached details if still fresh — frontend polls every 5 s,
	// so a 10 s TTL halves the API load without noticeable staleness.
	if d.cachedDetails != nil {
		if cd, ok := d.cachedDetails[mapping.Port]; ok && time.Since(cd.at) < dispatcharrDetailsTTL {
			return cd.data, nil
		}
	}

	status, err := d.fetchStatus(ctx, mapping)
	if err != nil {
		return nil, err
	}
	out := make([]dispatcharrStreamDetail, 0, len(status.Channels))
	for _, ch := range status.Channels {
		clients := make([]dispatcharrClientInfo, 0, len(ch.Clients))
		for _, c := range ch.Clients {
			clients = append(clients, dispatcharrClientInfo{
				ClientID:       c.ClientID,
				IPAddress:      c.IPAddress,
				UserAgent:      c.UserAgent,
				ConnectedSince: c.ConnectedSince,
			})
		}
		out = append(out, dispatcharrStreamDetail{
			ChannelID:      ch.ChannelID,
			StreamName:     ch.StreamName,
			ClientCount:    ch.ClientCount,
			AvgBitrateKbps: ch.AvgBitrateKbps,
			UptimeSeconds:  ch.Uptime,
			VideoCodec:     ch.VideoCodec,
			Resolution:     ch.Resolution,
			AudioCodec:     ch.AudioCodec,
			AudioChannels:  ch.AudioChannels,
			Clients:        clients,
		})
	}

	if d.cachedDetails == nil {
		d.cachedDetails = make(map[int]cachedDispatcharrDetails)
	}
	d.cachedDetails[mapping.Port] = cachedDispatcharrDetails{data: out, at: time.Now()}
	return out, nil
}

// _ compile-time checks — fail loudly if either interface drifts.
var _ ServiceDetailer = (*dispatcharrPoller)(nil)
var _ ServiceVerifier = (*dispatcharrPoller)(nil)

// forceRelogin drops the cached token for a port, so the next Poll/Details
// call will issue a fresh login. Used by the test endpoint so an operator
// can recover from credential changes without restarting the container.
func (d *dispatcharrPoller) forceRelogin(port int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.tokens, port)
}

// Verify performs a credential + connectivity check WITHOUT mutating any
// per-port byte state. Used by the Test button so test cycles don't leak
// bytes into the real session counter. Implements ServiceVerifier.
//
// We bypass the cached token and the per-port state map entirely — a fresh
// login goes to a temporary token variable that is discarded on return.
// We then do one /proxy/ts/status call with that token to confirm the
// account has API access; the response body is decoded but not retained.
func (d *dispatcharrPoller) Verify(ctx context.Context, mapping PortMapping) error {
	if mapping.Username == "" || mapping.Password == "" {
		return fmt.Errorf("dispatcharr: missing username/password in config")
	}
	body, _ := json.Marshal(dispatcharrLoginReq{
		Username: mapping.Username,
		Password: mapping.Password,
	})
	loginURL := dispatcharrURL(mapping, "/api/accounts/token/")
	req, err := http.NewRequestWithContext(ctx, "POST", loginURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dispatcharr: login status %d", resp.StatusCode)
	}
	var lr dispatcharrLoginResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return err
	}
	if lr.Access == "" {
		return fmt.Errorf("dispatcharr: login returned empty access token")
	}
	// One /proxy/ts/status call with the freshly minted token. We don't
	// keep the response — only the HTTP status matters for verification.
	_, code, err := d.getStatusWith(ctx, mapping, lr.Access)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("dispatcharr: status endpoint returned %d", code)
	}
	return nil
}

func init() {
	registerPoller(&dispatcharrPoller{}, "dispatcharr")
}
