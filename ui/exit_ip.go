package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"vpn-gateway-ui/netsec"
)

// The dashboard can show the VPN server address our tunnel connects to,
// but that is not always the address the outside world sees. Most
// providers NAT outbound traffic through the same address; PIA does not,
// so a tracker or website reports a different IP than the badge showed.
// The only way to know the outgoing address is to ask something on the
// internet, which is what this file does, and only when the user picks
// the "exit" badge mode.
//
// Cloudflare's trace endpoint is used because it is a plain text
// key=value response from an address every VPN tunnel can reach, needs no
// account, and returns nothing but connection facts. The request goes out
// through the tunnel like all other container traffic.
const exitIPLookupURL = "https://1.1.1.1/cdn-cgi/trace"

// exitIPRefreshInterval is how often the outgoing address is re-checked
// while the badge mode is on. The address only changes when the tunnel
// reconnects, and an endpoint change triggers an immediate re-check, so a
// quarter of an hour is frequent enough to stay honest without making a
// habit of calling out.
const exitIPRefreshInterval = 15 * time.Minute

// exitIPRetryInterval is the shorter wait after a failed lookup. A DNS or
// TLS blip should not keep the badge stale for a full refresh interval,
// and the previous address is kept meanwhile rather than blanked.
const exitIPRetryInterval = 60 * time.Second

// exitIPClient is a netsec safe client with no allowlist: the default
// policy permits public addresses and blocks private and loopback ones,
// which is exactly right here. Proxy is nil, so an attacker-set HTTP_PROXY
// cannot capture the lookup and answer with an address of its choosing.
var exitIPClient = netsec.NewSafeHTTPClient(10*time.Second, nil)

// exitIPCache holds the last looked-up outgoing address. endpoint records
// which VPN server the value was measured through, so reconnecting to a
// different server invalidates it rather than showing a stale address.
type exitIPCache struct {
	mu        sync.Mutex
	ip        string
	endpoint  string
	problem   string
	checkedAt time.Time
}

func (c *exitIPCache) get() (ip, problem, endpoint string, checkedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ip, c.problem, c.endpoint, c.checkedAt
}

func (c *exitIPCache) set(ip, problem, endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ip, c.problem, c.endpoint = ip, problem, endpoint
	c.checkedAt = time.Now()
}

// fail records a failed lookup. The last known address is kept: it is
// still the best answer available, and blanking the badge over one blip
// looks like the tunnel changed when it did not.
func (c *exitIPCache) fail(problem, endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.problem = problem
	c.endpoint = endpoint
	c.checkedAt = time.Now()
}

// stale reports whether a fresh lookup is due: no value yet, the tunnel
// now goes through a different server, or the cached value has aged out.
func (c *exitIPCache) stale(endpoint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checkedAt.IsZero() || c.endpoint != endpoint {
		return true
	}
	if c.problem != "" {
		return time.Since(c.checkedAt) >= exitIPRetryInterval
	}
	return time.Since(c.checkedAt) >= exitIPRefreshInterval
}

// parseTraceIP pulls the ip= value out of a Cloudflare trace body. The
// body is one key=value per line; everything other than ip= is ignored.
// Returns an error when the field is missing or not an address, so the
// dashboard can say the lookup failed instead of showing a fragment of
// someone else's response.
func parseTraceIP(body string) (string, error) {
	for _, line := range strings.Split(body, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "ip=")
		if !ok {
			continue
		}
		if net.ParseIP(value) == nil {
			return "", fmt.Errorf("exit ip: %q is not an IP address", value)
		}
		return value, nil
	}
	return "", fmt.Errorf("exit ip: no ip= field in the response")
}

// lookupExitIP asks the trace endpoint which address the request came
// from. client and url are parameters so tests can point it at a local
// stub; production callers use exitIPClient and exitIPLookupURL.
func lookupExitIP(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exit ip: status %d from the lookup", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return "", err
	}
	return parseTraceIP(string(body))
}

// refreshExitIP performs one lookup if the badge mode asks for it and the
// cached value is stale. Nothing leaves the container while the user has
// not chosen the exit mode: this is the whole reason the address is not
// shown by default.
func (app *App) refreshExitIP() {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		log.Printf("exit-ip: load config: %v", err)
		return
	}
	if !vpnIPDisplayWantsExit(cfg.VPNIPDisplay) {
		return
	}
	endpoint, _ := resolveVPNEndpoint(app.iface, app.vpnConfigPath)
	if endpoint == "" {
		// Tunnel is not up yet; a lookup now would either fail or, worse,
		// report the host's own address and look like a leak.
		return
	}
	if !app.exitIP.stale(endpoint) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ip, err := lookupExitIP(ctx, exitIPClient, exitIPLookupURL)
	if err != nil {
		app.exitIP.fail("the outgoing address could not be looked up", endpoint)
		log.Printf("exit-ip: lookup failed: %v", err)
		return
	}
	previous, _, _, _ := app.exitIP.get()
	app.exitIP.set(ip, "", endpoint)
	if previous != ip {
		log.Printf("exit-ip: traffic leaves through %s (VPN server %s)", ip, endpoint)
	}
}

// vpnIPDisplayWantsExit reports whether the configured badge mode includes
// the looked-up outgoing address.
func vpnIPDisplayWantsExit(mode string) bool {
	return mode == "exit"
}
