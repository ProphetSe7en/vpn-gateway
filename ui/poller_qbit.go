package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// qBitPoller implements ServicePoller for qBittorrent's WebUI API.
// This is the historical default — existing configs that have no Type
// field on their PortMapping will fall through to this poller via the
// empty-string key registered in init().
//
// Auth handling: qBit's WebUI defaults to requiring username/password
// (admin/adminadmin out of the box). vpn-gateway used to assume
// LocalHostAuth=false on localhost in the shared network namespace,
// which is true for some hotio-style setups but not the majority of
// users. Now we support both: empty Username/Password = no-auth
// (legacy), non-empty = login flow with SID cookie cache + 401 retry.
type qBitPoller struct {
	mu              sync.Mutex
	cookies         map[int]string    // port → SID cookie value
	authFailedUntil map[int]time.Time // port → time before which login attempts are skipped
	authFailCount   map[int]int       // port → consecutive auth failures (resets on success)
}

// qbitWriteClient is a dedicated HTTP client for qBit mutation calls
// (setPreferences). The shared httpClient has a 3 s timeout tuned for
// read-only transfer-info polls; writing a full preferences blob back
// to qBit can take longer on large configs, so writes get their own
// 15 s budget. The context timeout we pass in is advisory on top of
// this ceiling — the http.Client.Timeout is a wall-clock cap that
// applies to the entire request chain regardless of context.
var qbitWriteClient = &http.Client{Timeout: 15 * time.Second}

// authBackoffDuration is the cool-off applied after qBit rejects login
// enough times in a row that the next attempt would likely trip its
// built-in IP ban (5 failures by default). 15 min gives the ban window
// time to expire and throttles log spam on genuinely-wrong credentials.
const authBackoffDuration = 15 * time.Minute
const authBackoffThreshold = 3

// qBitTransferInfo mirrors qBit's /api/v2/transfer/info response shape.
// Only the four fields we care about are decoded. All counts are int64
// (matching qBit's native types) and are clamped to zero by the caller
// before being handed to the generic counter logic.
type qBitTransferInfo struct {
	DlInfoSpeed int64 `json:"dl_info_speed"`
	UpInfoSpeed int64 `json:"up_info_speed"`
	DlInfoData  int64 `json:"dl_info_data"` // session total downloaded
	UpInfoData  int64 `json:"up_info_data"` // session total uploaded
}

// Type returns the canonical type key for qBittorrent.
func (q *qBitPoller) Type() string { return "qbittorrent" }

// Poll fetches the current transfer info from qBittorrent. The auth flow
// runs only when the request fails with 401 AND credentials are
// configured — keeps the no-auth case (LocalHostAuth=false) on the same
// fast path as before. A non-nil error means the request failed; the
// caller will preserve the previously persisted counter values for this
// port so a brief auth blip doesn't reset the cumulative graphs.
func (q *qBitPoller) Poll(ctx context.Context, mapping PortMapping) (ServiceStats, error) {
	info, err := q.fetchTransferInfo(ctx, mapping)
	if err != nil {
		return ServiceStats{}, err
	}
	// Clamp negative values — qBit occasionally returns -1 on startup
	if info.DlInfoData < 0 {
		info.DlInfoData = 0
	}
	if info.UpInfoData < 0 {
		info.UpInfoData = 0
	}
	if info.DlInfoSpeed < 0 {
		info.DlInfoSpeed = 0
	}
	if info.UpInfoSpeed < 0 {
		info.UpInfoSpeed = 0
	}
	return ServiceStats{
		SessionRx: info.DlInfoData,
		SessionTx: info.UpInfoData,
		LiveRx:    info.DlInfoSpeed,
		LiveTx:    info.UpInfoSpeed,
		// Active left at 0 — qBit count requires a second API call and the
		// existing UI does not display it for qBit. Future enhancement.
	}, nil
}

// fetchTransferInfo issues GET /api/v2/transfer/info, transparently
// (re-)logging in if the request returns 401 and credentials are
// available. Single retry per call — if login itself returns 401 we
// surface the error rather than looping.
func (q *qBitPoller) fetchTransferInfo(ctx context.Context, mapping PortMapping) (qBitTransferInfo, error) {
	resp, err := q.do(ctx, mapping, "GET", "/api/v2/transfer/info", nil)
	if err != nil {
		return qBitTransferInfo{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		// Cookie may have expired (qBit invalidates after restart) or we
		// never logged in. Re-login and retry once.
		if mapping.Username == "" && mapping.Password == "" {
			return qBitTransferInfo{}, fmt.Errorf("qbit: 401 Unauthorized — qBit has auth enabled but no Username/Password is configured for this port")
		}
		if err := q.login(ctx, mapping); err != nil {
			return qBitTransferInfo{}, err
		}
		resp, err = q.do(ctx, mapping, "GET", "/api/v2/transfer/info", nil)
		if err != nil {
			return qBitTransferInfo{}, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return qBitTransferInfo{}, fmt.Errorf("qbit: unexpected status %d on /api/v2/transfer/info", resp.StatusCode)
	}
	var info qBitTransferInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return qBitTransferInfo{}, err
	}
	return info, nil
}

// do issues an HTTP request to qBit via the default httpClient (3 s).
// Appropriate for read paths (transfer/info, auth/login). See doWrite
// for mutations that need a longer timeout.
func (q *qBitPoller) do(ctx context.Context, mapping PortMapping, method, path string, body io.Reader) (*http.Response, error) {
	return q.doWith(ctx, mapping, method, path, body, httpClient)
}

// doWrite is do()'s counterpart for qBit mutations like setPreferences
// that can take longer than 3 s on instances with large configs. Uses
// qbitWriteClient (15 s ceiling) so a 10 s context budget from the
// auto-sync loop isn't silently capped to 3 s by the shared read client.
func (q *qBitPoller) doWrite(ctx context.Context, mapping PortMapping, method, path string, body io.Reader) (*http.Response, error) {
	return q.doWith(ctx, mapping, method, path, body, qbitWriteClient)
}

// doWith is the shared worker for do/doWrite. body may be nil. Caller
// is responsible for Close() on the returned Response when err is nil.
func (q *qBitPoller) doWith(ctx context.Context, mapping PortMapping, method, path string, body io.Reader, client *http.Client) (*http.Response, error) {
	u := fmt.Sprintf("http://localhost:%d%s", mapping.Port, path)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if sid := q.getSID(mapping.Port); sid != "" {
		req.AddCookie(&http.Cookie{Name: "SID", Value: sid})
	}
	if body != nil && method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return client.Do(req)
}

// login POSTs to /api/v2/auth/login and caches the returned SID cookie
// under the port. qBit's response semantics:
//   - 200 + body "Ok." → success, Set-Cookie: SID=…
//   - 200 + body "Fails." → wrong username/password
//   - 403 → IP banned (too many failures)
//
// Before attempting login, checks authBackoff: if the port has failed
// N consecutive times recently, skip the request entirely to avoid
// contributing to a qBit IP ban and to quiet log-spam on persistent
// wrong-creds. On success, clears the failure counter. On failure,
// increments the counter and arms the backoff window when the
// threshold is reached.
func (q *qBitPoller) login(ctx context.Context, mapping PortMapping) error {
	if err := q.checkAuthBackoff(mapping.Port); err != nil {
		return err
	}
	form := url.Values{}
	form.Set("username", mapping.Username)
	form.Set("password", mapping.Password)
	resp, err := q.do(ctx, mapping, "POST", "/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("qbit: login request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		q.recordAuthFailure(mapping.Port)
		return fmt.Errorf("qbit: 403 on login — IP may be banned (too many failed attempts)")
	}
	if resp.StatusCode != http.StatusOK {
		q.recordAuthFailure(mapping.Port)
		return fmt.Errorf("qbit: login returned status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if strings.TrimSpace(string(body)) != "Ok." {
		q.recordAuthFailure(mapping.Port)
		return fmt.Errorf("qbit: login failed — check Username and Password")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "SID" && c.Value != "" {
			q.setSID(mapping.Port, c.Value)
			q.clearAuthFailure(mapping.Port)
			return nil
		}
	}
	q.recordAuthFailure(mapping.Port)
	return fmt.Errorf("qbit: login succeeded but no SID cookie returned")
}

// checkAuthBackoff returns an error when the port is currently in the
// back-off window, so callers can short-circuit without hitting qBit.
// Expired windows are cleared on read.
func (q *qBitPoller) checkAuthBackoff(port int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.authFailedUntil == nil {
		return nil
	}
	until, ok := q.authFailedUntil[port]
	if !ok {
		return nil
	}
	if time.Now().Before(until) {
		remaining := time.Until(until).Round(time.Second)
		return fmt.Errorf("qbit: auth backoff active (%s remaining) — fix Username/Password and the next successful login will clear this", remaining)
	}
	delete(q.authFailedUntil, port)
	return nil
}

// recordAuthFailure bumps the consecutive-failure counter and arms the
// back-off window once the threshold is crossed. First few failures
// don't back off — legitimate password typos or transient issues
// shouldn't lock the user out for 15 min on the first mistake.
func (q *qBitPoller) recordAuthFailure(port int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.authFailCount == nil {
		q.authFailCount = make(map[int]int)
	}
	q.authFailCount[port]++
	if q.authFailCount[port] < authBackoffThreshold {
		return
	}
	if q.authFailedUntil == nil {
		q.authFailedUntil = make(map[int]time.Time)
	}
	q.authFailedUntil[port] = time.Now().Add(authBackoffDuration)
}

// clearAuthFailure resets the counter on a successful login. Called
// separately from setSID so an externally-forced cookie refresh (via
// Verify) can't be misread as a login attempt.
func (q *qBitPoller) clearAuthFailure(port int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.authFailCount, port)
	delete(q.authFailedUntil, port)
}

// SetListeningPort writes the given listening port into the qBit
// instance's preferences via /api/v2/app/setPreferences. Used by the
// auto-sync goroutine when PIA / Proton rotates the dynamic forwarded
// port. Same auth flow as Poll: try with cached SID, re-login on 401.
//
// qBit's setPreferences accepts a JSON-encoded "json" form field — only
// fields included are updated, so we touch listen_port without
// disturbing any other setting the admin has tuned. Returns nil on
// success, error otherwise (caller logs and retries on next file change).
func (q *qBitPoller) SetListeningPort(ctx context.Context, mapping PortMapping, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("qbit setPreferences: invalid port %d", port)
	}
	form := url.Values{}
	form.Set("json", fmt.Sprintf(`{"listen_port":%d}`, port))
	// Write path uses qbitWriteClient (15 s timeout) — setPreferences can
	// take longer than the 3 s read client allows on large configs.
	resp, err := q.doWrite(ctx, mapping, "POST", "/api/v2/app/setPreferences", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("qbit setPreferences: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if mapping.Username == "" && mapping.Password == "" {
			return fmt.Errorf("qbit setPreferences: 401 — qBit has auth enabled but no Username/Password configured")
		}
		if err := q.login(ctx, mapping); err != nil {
			return err
		}
		resp, err = q.doWrite(ctx, mapping, "POST", "/api/v2/app/setPreferences", strings.NewReader(form.Encode()))
		if err != nil {
			return fmt.Errorf("qbit setPreferences: %w", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbit setPreferences: status %d", resp.StatusCode)
	}
	return nil
}

// Verify exercises the auth + transfer-info path without recording
// stats — used by the Test button in Service Monitoring. Forces a
// fresh login so an existing-but-stale cookie can't fool the test.
func (q *qBitPoller) Verify(ctx context.Context, mapping PortMapping) error {
	q.setSID(mapping.Port, "") // invalidate cached cookie
	if mapping.Username != "" || mapping.Password != "" {
		if err := q.login(ctx, mapping); err != nil {
			return err
		}
	}
	resp, err := q.do(ctx, mapping, "GET", "/api/v2/transfer/info", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("qbit: 401 Unauthorized — qBit has auth enabled but no Username/Password is configured for this port")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbit: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// getSID / setSID guard the per-port cookie cache. Cookies are short
// strings and the map is small, so a single mutex is fine — Poll runs
// at most every 3 s per port.
func (q *qBitPoller) getSID(port int) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cookies[port]
}

func (q *qBitPoller) setSID(port int, sid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cookies == nil {
		q.cookies = make(map[int]string)
	}
	if sid == "" {
		delete(q.cookies, port)
		return
	}
	q.cookies[port] = sid
}

func init() {
	// Register under both the canonical type key and the empty-string key so
	// existing configs with no Type field on their PortMapping continue to
	// work as qBittorrent without any migration.
	p := &qBitPoller{}
	registerPoller(p, "qbittorrent", "")
}
