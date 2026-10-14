package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vpn-gateway-ui/netsec"
)

func TestParseTraceIP(t *testing.T) {
	body := "fl=123abc\nh=1.1.1.1\nip=203.0.113.10\nts=1760000000\nvisit_scheme=https\n"
	got, err := parseTraceIP(body)
	if err != nil || got != "203.0.113.10" {
		t.Fatalf("got (%q, %v), want 203.0.113.10", got, err)
	}
	if ip, err := parseTraceIP("ip=2001:db8::1\n"); err != nil || ip != "2001:db8::1" {
		t.Fatalf("IPv6: got (%q, %v)", ip, err)
	}
	for _, bad := range []string{"", "fl=123\nts=1\n", "ip=not-an-ip\n", "ip=\n"} {
		if _, err := parseTraceIP(bad); err == nil {
			t.Errorf("parseTraceIP(%q) accepted a bad body", bad)
		}
	}
}

// loopbackClient is a netsec safe client that may reach the test server.
// Production uses the same constructor with no allowlist, which permits
// public addresses and blocks private ones.
var loopbackClient = netsec.NewSafeHTTPClient(5*time.Second, loopbackAllowlist)

func TestLookupExitIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fl=abc\nip=203.0.113.10\nts=1\n"))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := lookupExitIP(ctx, loopbackClient, srv.URL)
	if err != nil || got != "203.0.113.10" {
		t.Fatalf("got (%q, %v), want 203.0.113.10", got, err)
	}
}

func TestLookupExitIPRejectsBadResponses(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"html page":    func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>nope</html>")) },
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := lookupExitIP(ctx, loopbackClient, srv.URL)
		cancel()
		srv.Close()
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The default client must refuse private destinations, so a redirect or a
// hostile DNS answer cannot turn the lookup into an internal probe.
func TestExitIPClientBlocksPrivateDestinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ip=203.0.113.10\n"))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := lookupExitIP(ctx, exitIPClient, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("err = %v, want a blocked-destination error", err)
	}
	if net.ParseIP("127.0.0.1") == nil {
		t.Fatal("sanity")
	}
}

func TestExitIPCacheStaleness(t *testing.T) {
	var c exitIPCache
	if !c.stale("203.0.113.1") {
		t.Fatal("an empty cache must be stale")
	}
	c.set("203.0.113.10", "", "203.0.113.1")
	if c.stale("203.0.113.1") {
		t.Fatal("a fresh value for the same endpoint must not be stale")
	}
	if !c.stale("203.0.113.2") {
		t.Fatal("a different VPN server must invalidate the value")
	}
	ip, problem, endpoint, checkedAt := c.get()
	if ip != "203.0.113.10" || problem != "" || endpoint != "203.0.113.1" || checkedAt.IsZero() {
		t.Fatalf("unexpected cache contents: %q %q %q %v", ip, problem, endpoint, checkedAt)
	}
}

func TestVPNIPDisplayWantsExit(t *testing.T) {
	for mode, want := range map[string]bool{"exit": true, "server": false, "tunnel": false, "both": false, "off": false, "": false} {
		if got := vpnIPDisplayWantsExit(mode); got != want {
			t.Errorf("mode %q: got %v, want %v", mode, got, want)
		}
	}
}
