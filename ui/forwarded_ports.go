package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StaticPortRedirect is one entry from the VPN_PORT_REDIRECTS env var
// (set in the Docker template, parsed by hotio for iptables rules).
// These are user-defined static forwards typical of TorGuard / Mullvad-PF
// / AirVPN / generic WireGuard where the user reserves a port at the
// provider and hard-codes it in the container config.
type StaticPortRedirect struct {
	VPNPort       int    `json:"vpnPort"`
	ContainerPort int    `json:"containerPort"`
	Proto         string `json:"proto"`
}

// parseStaticPortRedirects parses hotio's VPN_PORT_REDIRECTS env-var
// format:
//
//	portA@portB/proto              → {VPNPort:A, ContainerPort:B, Proto:"proto"}
//	portA@portB/tcp,portC@portD/udp → two entries
//
// Malformed entries are skipped silently rather than erroring out — hotio
// is lenient here, and a partial list is more useful than nothing when
// the admin fat-fingered one entry.
func parseStaticPortRedirects(env string) []StaticPortRedirect {
	env = strings.TrimSpace(env)
	if env == "" {
		return nil
	}
	var out []StaticPortRedirect
	for _, entry := range strings.Split(env, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		proto := "tcp"
		if slash := strings.LastIndexByte(entry, '/'); slash >= 0 {
			proto = strings.ToLower(strings.TrimSpace(entry[slash+1:]))
			entry = entry[:slash]
		}
		at := strings.IndexByte(entry, '@')
		if at < 0 {
			continue
		}
		vpnPort, err1 := strconv.Atoi(strings.TrimSpace(entry[:at]))
		containerPort, err2 := strconv.Atoi(strings.TrimSpace(entry[at+1:]))
		if err1 != nil || err2 != nil {
			continue
		}
		if vpnPort < 1 || vpnPort > 65535 || containerPort < 1 || containerPort > 65535 {
			continue
		}
		out = append(out, StaticPortRedirect{
			VPNPort:       vpnPort,
			ContainerPort: containerPort,
			Proto:         proto,
		})
	}
	return out
}

// readDynamicForwardedPort reads the /config/wireguard/forwarded_port
// file populated by hotio's PIA / Proton port-forward service. The file
// is a single integer on its own line (trailing newline OK). Returns 0
// when the file is missing (no PIA/Proton PF active) or malformed.
func readDynamicForwardedPort(wireguardDir string) int {
	data, err := os.ReadFile(filepath.Join(wireguardDir, "forwarded_port"))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// readPIARotatesAt parses the expires_at timestamp from hotio's PIA
// persist state file at /config/wireguard/<iface>.persist.json. Present
// only when VPN_PIA_PORT_FORWARD_PERSIST=true. Returns the zero time on
// any error — callers skip the countdown gracefully when present.
//
// Tries common schemas seen across hotio versions:
//   - Flat: {"expires_at": "..."}
//   - Nested: {"payload": {"expires_at": "..."}}
//
// The full signed-payload format (base64-encoded payload + signature) is
// not parsed here — if a future hotio release switches to it the
// countdown just disappears until this code catches up, which is less
// bad than displaying a wrong value.
func readPIARotatesAt(wireguardDir, iface string) time.Time {
	if iface == "" {
		iface = "wg0"
	}
	data, err := os.ReadFile(filepath.Join(wireguardDir, iface+".persist.json"))
	if err != nil {
		return time.Time{}
	}
	var flat struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &flat); err == nil && flat.ExpiresAt != "" {
		if t := parseExpiresAt(flat.ExpiresAt); !t.IsZero() {
			return t
		}
	}
	var nested struct {
		Payload struct {
			ExpiresAt string `json:"expires_at"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &nested); err == nil && nested.Payload.ExpiresAt != "" {
		if t := parseExpiresAt(nested.Payload.ExpiresAt); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func parseExpiresAt(s string) time.Time {
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
