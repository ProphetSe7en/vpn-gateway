package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"vpn-gateway-ui/netsec"
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
// (legacy), non-empty = login flow with session cookie cache and a
// single re-login when qBit rejects the session (401 or 403).
//
// qBittorrent 5.2.0 changed the WebUI login contract. Both shapes are
// supported:
//
//	                    up to 5.1         5.2+
//	good login          200 + "Ok."       204, empty body
//	wrong password      200 + "Fails."    401
//	session cookie      SID               QBT_SID_<webui-port>
//	banned client       403               403
//	no/expired session  403               403
type qBitPoller struct {
	mu              sync.Mutex
	cookies         map[int]qbitSession       // port → session cookie
	authFailedUntil map[qbitAuthKey]time.Time // credentials → time before which login attempts are skipped
	authFailCount   map[qbitAuthKey]int       // credentials → consecutive auth failures (resets on success)
}

// qbitAuthKey scopes the login backoff to one port AND one set of
// credentials. Correcting the Username/Password (saved, or typed into the
// Test dialog) gets a fresh start immediately instead of waiting out a
// backoff armed by the old, wrong values. The credentials are hashed so
// the map never holds a plaintext password.
type qbitAuthKey struct {
	port int
	cred [sha256.Size]byte
}

func newQBitAuthKey(mapping PortMapping) qbitAuthKey {
	return qbitAuthKey{
		port: mapping.Port,
		cred: sha256.Sum256([]byte(mapping.Username + "\x00" + mapping.Password)),
	}
}

// qbitSession is a cached WebUI session cookie. The name is stored with
// the value because qBit 5.2+ names the cookie after its own WebUI port
// (QBT_SID_8080), which can differ from the port we dial when the
// container remaps it, so the name has to be replayed as offered.
type qbitSession struct {
	name  string
	value string
}

// isQBitSessionCookie reports whether a Set-Cookie name is qBit's WebUI
// session cookie: "SID" up to 5.1, "QBT_SID_<digits>" from 5.2.
func isQBitSessionCookie(name string) bool {
	if name == "SID" {
		return true
	}
	port, ok := strings.CutPrefix(name, "QBT_SID_")
	if !ok || port == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// qbitSessionRejected reports whether an API response means the session
// cookie is missing or no longer valid. qBit answers unauthenticated API
// calls with 403; 401 is kept for CSRF / Host-header rejections and older
// builds.
func qbitSessionRejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// qbitWriteClient is a dedicated HTTP client for qBit mutation calls
// (setPreferences). The shared httpClient has a 3 s timeout tuned for
// read-only transfer-info polls; writing a full preferences blob back
// to qBit can take longer on large configs, so writes get their own
// 15 s budget. The context timeout we pass in is advisory on top of
// this ceiling — the http.Client.Timeout is a wall-clock cap that
// applies to the entire request chain regardless of context.
//
// Like httpClient it is a netsec safe client (not a bare http.Client)
// sharing the loopback allowlist, so the credential-bearing
// setPreferences write also gets Proxy: nil and per-request IP
// re-validation.
var qbitWriteClient = netsec.NewSafeHTTPClient(15*time.Second, loopbackAllowlist)

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
// runs only when qBit rejects the session (401 or 403) AND credentials are
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
// (re-)logging in if qBit rejects the session (401 or 403) and
// credentials are available. Single retry per call; if login itself
// fails we surface the error rather than looping.
func (q *qBitPoller) fetchTransferInfo(ctx context.Context, mapping PortMapping) (qBitTransferInfo, error) {
	resp, err := q.do(ctx, mapping, "GET", "/api/v2/transfer/info", nil)
	if err != nil {
		return qBitTransferInfo{}, err
	}
	if qbitSessionRejected(resp.StatusCode) {
		resp.Body.Close()
		// Cookie may have expired (qBit invalidates after restart) or we
		// never logged in. Re-login and retry once.
		if mapping.Username == "" && mapping.Password == "" {
			return qBitTransferInfo{}, errQBitNoCredentials(resp.StatusCode)
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
	// Dial the literal IPv4 loopback rather than "localhost": the safe
	// client's dialer only attempts the first resolved IP, so a host that
	// resolves "localhost" to ::1 first would break IPv4-only services.
	u := fmt.Sprintf("http://127.0.0.1:%d%s", mapping.Port, path)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if s := q.getSession(mapping.Port); s.value != "" {
		req.AddCookie(&http.Cookie{Name: s.name, Value: s.value})
	}
	if body != nil && method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return client.Do(req)
}

// errQBitNoCredentials is returned when qBit rejects the session but no
// Username/Password is configured, so there is nothing to log in with.
func errQBitNoCredentials(status int) error {
	return fmt.Errorf("qbit: status %d, qBittorrent requires a login but no Username/Password is configured for this port", status)
}

// login POSTs to /api/v2/auth/login and caches the returned session
// cookie under the port. See the qBitPoller doc comment for the response
// shapes of qBit up to 5.1 and 5.2+; both are handled here:
//   - 2xx with a session cookie (SID or QBT_SID_<port>) → success
//   - 200 + body "Fails." (up to 5.1) or 401 (5.2+) → wrong username/password
//   - 403 → client IP banned after too many failed attempts
//
// Before attempting login, checks authBackoff: if these credentials have
// been rejected N consecutive times recently, skip the request entirely
// to avoid contributing to a qBit IP ban and to quiet log-spam on
// persistent wrong-creds. On success, clears the failure counter. Only
// rejections of the credentials (wrong password, banned) are counted;
// other statuses such as a 5xx while qBit is starting are not.
func (q *qBitPoller) login(ctx context.Context, mapping PortMapping) error {
	key := newQBitAuthKey(mapping)
	if err := q.checkAuthBackoff(key); err != nil {
		return err
	}
	// Drop the cached cookie first. qBit up to 5.1 answers a login that
	// carries a still-valid session with "Ok." and no new cookie, which
	// would leave us without a session to cache.
	q.setSession(mapping.Port, qbitSession{})
	form := url.Values{}
	form.Set("username", mapping.Username)
	form.Set("password", mapping.Password)
	resp, err := q.do(ctx, mapping, "POST", "/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("qbit: login request: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden:
		q.recordAuthFailure(key)
		return fmt.Errorf("qbit: 403 on login, IP may be banned (too many failed attempts)")
	case resp.StatusCode == http.StatusUnauthorized:
		q.recordAuthFailure(key)
		return fmt.Errorf("qbit: login failed, check Username and Password")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("qbit: login returned status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if strings.TrimSpace(string(body)) == "Fails." {
		q.recordAuthFailure(key)
		return fmt.Errorf("qbit: login failed, check Username and Password")
	}
	for _, c := range resp.Cookies() {
		if isQBitSessionCookie(c.Name) && c.Value != "" {
			q.setSession(mapping.Port, qbitSession{name: c.Name, value: c.Value})
			q.clearAuthFailure(key)
			return nil
		}
	}
	// Not counted as an auth failure: the credentials were accepted, so
	// arming the backoff would blame the Username/Password for it.
	return fmt.Errorf("qbit: login accepted but no session cookie was returned")
}

// checkAuthBackoff returns an error when these credentials are currently
// in the back-off window, so callers can short-circuit without hitting
// qBit. Expired windows are cleared on read; the failure count is kept,
// so after the window each further rejection re-arms it straight away
// (at most one login attempt per window with the same wrong values).
func (q *qBitPoller) checkAuthBackoff(key qbitAuthKey) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	until, ok := q.authFailedUntil[key]
	if !ok {
		return nil
	}
	if time.Now().Before(until) {
		remaining := time.Until(until).Round(time.Second)
		return fmt.Errorf("qbit: login paused for %s after repeated failures, changing the Username or Password allows a new attempt right away", remaining)
	}
	delete(q.authFailedUntil, key)
	return nil
}

// recordAuthFailure bumps the consecutive-failure counter and arms the
// back-off window once the threshold is crossed. First few failures
// don't back off — legitimate password typos or transient issues
// shouldn't lock the user out for 15 min on the first mistake.
func (q *qBitPoller) recordAuthFailure(key qbitAuthKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.authFailCount == nil {
		q.authFailCount = make(map[qbitAuthKey]int)
	}
	q.authFailCount[key]++
	if q.authFailCount[key] < authBackoffThreshold {
		return
	}
	if q.authFailedUntil == nil {
		q.authFailedUntil = make(map[qbitAuthKey]time.Time)
	}
	q.authFailedUntil[key] = time.Now().Add(authBackoffDuration)
}

// clearAuthFailure resets the state for every credential set on the port
// after a successful login, so entries for old wrong values don't linger.
// Called separately from setSession so an externally-forced cookie
// refresh (via Verify) can't be misread as a login attempt.
func (q *qBitPoller) clearAuthFailure(key qbitAuthKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for k := range q.authFailCount {
		if k.port == key.port {
			delete(q.authFailCount, k)
		}
	}
	for k := range q.authFailedUntil {
		if k.port == key.port {
			delete(q.authFailedUntil, k)
		}
	}
}

// SetListeningPort writes the given listening port into the qBit
// instance's preferences via /api/v2/app/setPreferences. Used by the
// auto-sync goroutine when PIA / Proton rotates the dynamic forwarded
// port. Same auth flow as Poll: try with the cached session, re-login
// once when qBit rejects it (401 or 403).
//
// qBit's setPreferences accepts a JSON-encoded "json" form field — only
// fields included are updated, so we touch listen_port without
// disturbing any other setting the admin has tuned. Success is any 2xx:
// 200 up to 5.1, 204 from 5.2. Returns nil on success, error otherwise
// (caller logs and retries on the next tick).
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
	if qbitSessionRejected(resp.StatusCode) {
		resp.Body.Close()
		if mapping.Username == "" && mapping.Password == "" {
			return errQBitNoCredentials(resp.StatusCode)
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
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("qbit setPreferences: status %d", resp.StatusCode)
	}
	return nil
}

// Verify exercises the auth + transfer-info path without recording
// stats — used by the Test button in Service Monitoring. Forces a
// fresh login so an existing-but-stale cookie can't fool the test.
func (q *qBitPoller) Verify(ctx context.Context, mapping PortMapping) error {
	q.setSession(mapping.Port, qbitSession{}) // invalidate cached cookie
	hasCredentials := mapping.Username != "" || mapping.Password != ""
	if hasCredentials {
		if err := q.login(ctx, mapping); err != nil {
			return err
		}
	}
	resp, err := q.do(ctx, mapping, "GET", "/api/v2/transfer/info", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if qbitSessionRejected(resp.StatusCode) {
		if !hasCredentials {
			return errQBitNoCredentials(resp.StatusCode)
		}
		return fmt.Errorf("qbit: status %d after a successful login, qBittorrent did not accept the session", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbit: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// getSession / setSession guard the per-port cookie cache. Cookies are
// short strings and the map is small, so a single mutex is fine; Poll
// runs at most every 3 s per port. An empty session clears the entry.
func (q *qBitPoller) getSession(port int) qbitSession {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cookies[port]
}

func (q *qBitPoller) setSession(port int, s qbitSession) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cookies == nil {
		q.cookies = make(map[int]qbitSession)
	}
	if s.value == "" {
		delete(q.cookies, port)
		return
	}
	q.cookies[port] = s
}

func init() {
	// Register under both the canonical type key and the empty-string key so
	// existing configs with no Type field on their PortMapping continue to
	// work as qBittorrent without any migration.
	p := &qBitPoller{}
	registerPoller(p, "qbittorrent", "")
}
