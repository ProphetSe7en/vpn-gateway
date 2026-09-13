package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeQBit emulates the qBittorrent WebUI API in either the pre-5.2
// shape (200 "Ok." + SID cookie) or the 5.2+ shape (204 + QBT_SID_<port>
// cookie, 401 on bad credentials). Unauthenticated API calls get 403 in
// both, matching qBit's WebApplication::doProcessRequest.
type fakeQBit struct {
	v52        bool
	username   string
	password   string
	cookieName string

	mu         sync.Mutex
	session    string
	logins     int
	listenPort int
}

func newFakeQBit(t *testing.T, v52 bool) (*fakeQBit, PortMapping) {
	t.Helper()
	f := &fakeQBit{v52: v52, username: "admin", password: "correct-horse", cookieName: "SID"}
	if v52 {
		// Named after qBit's own WebUI port, deliberately not the port we
		// dial, so the test proves the name is replayed as offered.
		f.cookieName = "QBT_SID_8080"
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	return f, PortMapping{Port: port, Name: "qbit", Type: "qbittorrent", Username: f.username, Password: f.password}
}

func (f *fakeQBit) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/v2/auth/login" {
		_ = r.ParseForm()
		f.logins++
		if r.PostForm.Get("username") != f.username || r.PostForm.Get("password") != f.password {
			if f.v52 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("Fails."))
			return
		}
		f.session = "session-" + strconv.Itoa(f.logins)
		http.SetCookie(w, &http.Cookie{Name: f.cookieName, Value: f.session, Path: "/", HttpOnly: true})
		if f.v52 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("Ok."))
		return
	}
	c, err := r.Cookie(f.cookieName)
	if err != nil || f.session == "" || c.Value != f.session {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/api/v2/transfer/info":
		_ = json.NewEncoder(w).Encode(qBitTransferInfo{DlInfoSpeed: 10, UpInfoSpeed: 20, DlInfoData: 30, UpInfoData: 40})
	case "/api/v2/app/setPreferences":
		_ = r.ParseForm()
		var prefs struct {
			ListenPort int `json:"listen_port"`
		}
		_ = json.Unmarshal([]byte(r.PostForm.Get("json")), &prefs)
		f.listenPort = prefs.ListenPort
		if f.v52 {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// expireSession simulates a qBit restart: the cached cookie stops working.
func (f *fakeQBit) expireSession() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.session = ""
}

func (f *fakeQBit) state() (logins, listenPort int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.listenPort
}

var qbitVersions = []struct {
	name string
	v52  bool
}{
	{"qBit up to 5.1", false},
	{"qBit 5.2+", true},
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestQBitSetListeningPortLogsInOn403(t *testing.T) {
	for _, v := range qbitVersions {
		t.Run(v.name, func(t *testing.T) {
			f, mapping := newFakeQBit(t, v.v52)
			q := &qBitPoller{}
			if err := q.SetListeningPort(testCtx(t), mapping, 31506); err != nil {
				t.Fatalf("SetListeningPort: %v", err)
			}
			logins, port := f.state()
			if port != 31506 {
				t.Fatalf("listen_port = %d, want 31506", port)
			}
			if logins != 1 {
				t.Fatalf("logins = %d, want 1", logins)
			}
		})
	}
}

func TestQBitPollReusesSessionAndRecoversAfterExpiry(t *testing.T) {
	for _, v := range qbitVersions {
		t.Run(v.name, func(t *testing.T) {
			f, mapping := newFakeQBit(t, v.v52)
			q := &qBitPoller{}
			for i := 0; i < 3; i++ {
				stats, err := q.Poll(testCtx(t), mapping)
				if err != nil {
					t.Fatalf("Poll %d: %v", i, err)
				}
				if stats.LiveRx != 10 || stats.SessionTx != 40 {
					t.Fatalf("unexpected stats: %+v", stats)
				}
			}
			if logins, _ := f.state(); logins != 1 {
				t.Fatalf("logins after 3 polls = %d, want 1 (session should be reused)", logins)
			}
			f.expireSession()
			if _, err := q.Poll(testCtx(t), mapping); err != nil {
				t.Fatalf("Poll after session expiry: %v", err)
			}
			if logins, _ := f.state(); logins != 2 {
				t.Fatalf("logins after expiry = %d, want 2", logins)
			}
		})
	}
}

func TestQBitWrongPasswordArmsBackoff(t *testing.T) {
	for _, v := range qbitVersions {
		t.Run(v.name, func(t *testing.T) {
			f, mapping := newFakeQBit(t, v.v52)
			mapping.Password = "wrong"
			q := &qBitPoller{}
			for i := 0; i < authBackoffThreshold; i++ {
				err := q.Verify(testCtx(t), mapping)
				if err == nil || !strings.Contains(err.Error(), "check Username and Password") {
					t.Fatalf("attempt %d: err = %v, want wrong-credentials error", i, err)
				}
			}
			err := q.Verify(testCtx(t), mapping)
			if err == nil || !strings.Contains(err.Error(), "login paused") {
				t.Fatalf("err = %v, want backoff error", err)
			}
			if logins, _ := f.state(); logins != authBackoffThreshold {
				t.Fatalf("logins = %d, want %d (backoff must stop further attempts)", logins, authBackoffThreshold)
			}

			// Correcting the password must not wait out the backoff armed
			// by the wrong one.
			mapping.Password = f.password
			if err := q.Verify(testCtx(t), mapping); err != nil {
				t.Fatalf("Verify with corrected password: %v", err)
			}
			// A success clears the old wrong-password state for the port.
			mapping.Password = "wrong"
			err = q.Verify(testCtx(t), mapping)
			if err == nil || !strings.Contains(err.Error(), "check Username and Password") {
				t.Fatalf("err = %v, want a fresh wrong-credentials error, not a lingering backoff", err)
			}
		})
	}
}

func TestQBitServerErrorOnLoginDoesNotArmBackoff(t *testing.T) {
	var logins int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/v2/auth/login" {
			logins++
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	mapping := PortMapping{Port: port, Name: "qbit", Type: "qbittorrent", Username: "admin", Password: "pw"}
	q := &qBitPoller{}
	for i := 0; i < authBackoffThreshold+2; i++ {
		err := q.Verify(testCtx(t), mapping)
		if err == nil || strings.Contains(err.Error(), "login paused") {
			t.Fatalf("attempt %d: err = %v, want a status error without backoff", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if logins != authBackoffThreshold+2 {
		t.Fatalf("logins = %d, want %d", logins, authBackoffThreshold+2)
	}
}

func TestQBitVerifySucceeds(t *testing.T) {
	for _, v := range qbitVersions {
		t.Run(v.name, func(t *testing.T) {
			_, mapping := newFakeQBit(t, v.v52)
			q := &qBitPoller{}
			if err := q.Verify(testCtx(t), mapping); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

func TestQBitNoCredentialsReportsMissingLogin(t *testing.T) {
	_, mapping := newFakeQBit(t, true)
	mapping.Username, mapping.Password = "", ""
	q := &qBitPoller{}
	for name, call := range map[string]func() error{
		"Poll":             func() error { _, err := q.Poll(testCtx(t), mapping); return err },
		"SetListeningPort": func() error { return q.SetListeningPort(testCtx(t), mapping, 31506) },
		"Verify":           func() error { return q.Verify(testCtx(t), mapping) },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "no Username/Password is configured") {
			t.Errorf("%s: err = %v, want missing-credentials error", name, err)
		}
	}
}

func TestIsQBitSessionCookie(t *testing.T) {
	cases := map[string]bool{
		"SID":           true,
		"QBT_SID_8080":  true,
		"QBT_SID_1":     true,
		"QBT_SID_":      false,
		"QBT_SID_80a":   false,
		"QBT_SID":       false,
		"sid":           false,
		"XSID":          false,
		"QBT_SID_8080x": false,
	}
	for name, want := range cases {
		if got := isQBitSessionCookie(name); got != want {
			t.Errorf("isQBitSessionCookie(%q) = %v, want %v", name, got, want)
		}
	}
}
