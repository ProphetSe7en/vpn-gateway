package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// sabPoller implements ServicePoller for SABnzbd's JSON API.
//
// SABnzbd has no per-session byte counter equivalent to qBittorrent's
// dl_info_data. The closest thing is /api?mode=server_stats which returns
// a lifetime cumulative total persisted in SABnzbd's own database — it
// keeps growing across SAB restarts and is never reset.
//
// We use that lifetime counter ONLY as a delta source. Per the design
// agreed in vpn-gateway v1.3.0:
//   - First successful poll captures SAB's current total as a baseline
//     and adds zero bytes to our session counter.
//   - Subsequent polls compute (current - lastTotal) and add that delta.
//   - Gap detection: if more than sabGapThreshold has elapsed since the
//     last successful poll, we treat the next poll as a fresh baseline
//     instead of trusting the delta. This handles scenarios like SAB
//     being moved to a different network for hours/days and then coming
//     back behind vpn-gateway — without it, the entire away-period
//     download volume would be falsely attributed to vpn-gateway.
//
// What the user sees as "Total" is therefore exclusively bytes that
// vpn-gateway observed flowing through SAB during this monitoring period,
// matching the mental model "we register the data, we don't pull SAB's
// historical numbers".
type sabPoller struct {
	mu    sync.Mutex
	state map[int]*sabPortState // keyed by mapping.Port
}

// sabPortState is per-configured-port internal state. It does NOT survive
// vpn-gateway restarts on purpose — the gap-detection branch handles the
// post-restart case identically to a long-gap scenario, so persisting it
// would buy nothing and add migration headaches.
type sabPortState struct {
	sessionBytes int64     // our running counter — what we display as Total
	lastTotal    int64     // last seen value of SAB's lifetime server_stats.total
	lastPollAt   time.Time // wall-clock time of the last successful poll
}

// sabGapThreshold is how long we tolerate between successful polls before
// the next poll is treated as a fresh baseline rather than a delta source.
// Normal poll interval is 3 s; 60 s gives ~20 missed polls of slack for
// transient network blips before we rebaseline.
const sabGapThreshold = 60 * time.Second

// sabQueueResp is the slice of /api?mode=queue we care about. SAB returns
// kbpersec as a string for historical reasons.
type sabQueueResp struct {
	Queue struct {
		KBPerSec  string `json:"kbpersec"`
		NoOfSlots int    `json:"noofslots"`
	} `json:"queue"`
}

// sabServerStatsResp is the slice of /api?mode=server_stats we care about.
// Top-level Total is bytes summed across all configured news servers.
type sabServerStatsResp struct {
	Total int64 `json:"total"`
}

// Type returns the canonical type key for SABnzbd.
func (s *sabPoller) Type() string { return "sabnzbd" }

// Poll fetches queue + server_stats from SABnzbd and returns aggregate
// stats with our self-maintained session counter (NOT SAB's lifetime total).
func (s *sabPoller) Poll(ctx context.Context, mapping PortMapping) (ServiceStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if mapping.APIKey == "" {
		return ServiceStats{}, fmt.Errorf("sabnzbd: missing API key in config")
	}

	queue, err := s.fetchQueue(ctx, mapping)
	if err != nil {
		return ServiceStats{}, err
	}
	stats, err := s.fetchServerStats(ctx, mapping)
	if err != nil {
		return ServiceStats{}, err
	}

	if s.state == nil {
		s.state = make(map[int]*sabPortState)
	}
	ps, ok := s.state[mapping.Port]
	if !ok {
		ps = &sabPortState{}
		s.state[mapping.Port] = ps
	}

	now := time.Now()

	// Three branches for the cumulative counter update:
	//
	//   1. First poll ever in this process — no baseline yet, just record.
	//   2. Gap too long (vpn-gateway was down, SAB was unreachable, or SAB
	//      was moved to another network and is back) — rebaseline to avoid
	//      attributing the away-period delta to vpn-gateway.
	//   3. Counter went backwards — should never happen with server_stats.total
	//      since SAB persists it, but we defensively rebaseline rather than
	//      add a negative delta.
	//   4. Normal — add the delta to our session counter.
	switch {
	case ps.lastPollAt.IsZero():
		// First poll: establish baseline only.
	case now.Sub(ps.lastPollAt) > sabGapThreshold:
		// Gap detected: rebaseline.
	case stats.Total < ps.lastTotal:
		// Backwards counter: rebaseline rather than panic.
	default:
		ps.sessionBytes += stats.Total - ps.lastTotal
	}
	ps.lastTotal = stats.Total
	ps.lastPollAt = now

	// SAB returns kbpersec as a string in KiB/s (despite the field name —
	// SAB internally uses powers of 2, verified by matching wg0's binary
	// counters byte-for-byte during live testing). Multiply by 1024 to get
	// bytes/sec. Empty or unparseable values fall back to 0 — no error,
	// just no live rate this poll.
	var liveRx int64
	if queue.Queue.KBPerSec != "" {
		if kb, err := strconv.ParseFloat(queue.Queue.KBPerSec, 64); err == nil && kb > 0 {
			liveRx = int64(kb * 1024)
		}
	}

	return ServiceStats{
		SessionRx: ps.sessionBytes,
		SessionTx: 0, // SAB is download-only from vpn-gateway's perspective
		LiveRx:    liveRx,
		LiveTx:    0,
		Active:    queue.Queue.NoOfSlots,
	}, nil
}

// Verify performs a credential + connectivity check WITHOUT mutating any
// per-port byte state. Implements ServiceVerifier so the Test button does
// not leak its check cycle into the live counter.
func (s *sabPoller) Verify(ctx context.Context, mapping PortMapping) error {
	if mapping.APIKey == "" {
		return fmt.Errorf("sabnzbd: missing API key in config")
	}
	// One queue call is sufficient — it will fail with a clear API error
	// (HTTP 200 + JSON error body) on bad key, and a connection error if
	// SAB is unreachable. We don't update any state from the response.
	_, err := s.fetchQueue(ctx, mapping)
	return err
}

// fetchQueue calls /api?mode=queue. SAB returns errors as HTTP 200 with
// `{"status": false, "error": "..."}` for some failure modes (e.g. wrong
// API key), so we sniff for that pattern after decoding.
func (s *sabPoller) fetchQueue(ctx context.Context, mapping PortMapping) (*sabQueueResp, error) {
	body, err := s.get(ctx, mapping, "queue")
	if err != nil {
		return nil, err
	}
	if errMsg := sabExtractError(body); errMsg != "" {
		return nil, fmt.Errorf("sabnzbd: %s", errMsg)
	}
	var q sabQueueResp
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// fetchServerStats calls /api?mode=server_stats and returns the top-level
// total only — we don't need the per-server breakdown.
func (s *sabPoller) fetchServerStats(ctx context.Context, mapping PortMapping) (*sabServerStatsResp, error) {
	body, err := s.get(ctx, mapping, "server_stats")
	if err != nil {
		return nil, err
	}
	if errMsg := sabExtractError(body); errMsg != "" {
		return nil, fmt.Errorf("sabnzbd: %s", errMsg)
	}
	var st sabServerStatsResp
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// sabMaxBody caps how many bytes we will read from a single SAB response.
// 1 MiB is generous for both endpoints we use — server_stats is tiny, queue
// can grow with very large SAB queues but stays well under this. The cap
// exists to bound memory usage if SAB ever returns a malformed/oversized
// payload, NOT as a normal-operation constraint.
const sabMaxBody = 1 << 20

// get is the shared HTTP helper for both queue and server_stats. Returns
// the raw response body so the caller can sniff for SAB's HTTP-200 error
// envelope before decoding into a typed struct.
//
// SECURITY: the API key is included in the request URL via url.Values
// encoding (correct percent-escaping). The URL string is never logged or
// returned in any error message — only HTTP status codes and SAB's own
// error envelope text are surfaced to callers, so an API key cannot leak
// through poller errors.
func (s *sabPoller) get(ctx context.Context, mapping PortMapping, mode string) ([]byte, error) {
	q := url.Values{}
	q.Set("mode", mode)
	q.Set("output", "json")
	q.Set("apikey", mapping.APIKey)
	full := fmt.Sprintf("http://localhost:%d/api?%s", mapping.Port, q.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", full, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sabnzbd: unexpected status %d", resp.StatusCode)
	}
	// LimitReader caps reads at sabMaxBody+1 so a too-large body is detected
	// without ever holding more than sabMaxBody+1 bytes in memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, sabMaxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > sabMaxBody {
		return nil, fmt.Errorf("sabnzbd: response too large")
	}
	return body, nil
}

// sabExtractError sniffs for SAB's `{"status": false, "error": "..."}`
// envelope. Returns the error string when present, empty string otherwise.
// We deliberately use a tiny anonymous struct rather than full schema
// matching so it works regardless of which mode= was called.
func sabExtractError(body []byte) string {
	var probe struct {
		Status *bool  `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if probe.Status != nil && !*probe.Status && probe.Error != "" {
		return probe.Error
	}
	return ""
}

// _ compile-time checks — fail loudly if either interface drifts.
var _ ServicePoller = (*sabPoller)(nil)
var _ ServiceVerifier = (*sabPoller)(nil)

func init() {
	registerPoller(&sabPoller{}, "sabnzbd")
}
