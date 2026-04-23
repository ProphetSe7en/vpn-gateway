package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"vpn-gateway-ui/netsec"
)

var (
	validTime = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	// Either a range (mon-fri) OR a comma list (mon,wed,fri), not mixed
	validDays = regexp.MustCompile(`^(mon|tue|wed|thu|fri|sat|sun)(-(mon|tue|wed|thu|fri|sat|sun))?$|^(mon|tue|wed|thu|fri|sat|sun)(,(mon|tue|wed|thu|fri|sat|sun))*$`)
)

type ScheduleRule struct {
	Time string `json:"time"`
	Down int    `json:"down"`
	Up   int    `json:"up"`
	Days string `json:"days"`
}

type PortMapping struct {
	Port  int    `json:"port"`            // WebUI port (e.g. 7073)
	Name  string `json:"name"`            // Display name (e.g. qBit-movies)
	Color string `json:"color,omitempty"` // Custom hex color (e.g. #f0883e)

	// Type selects which ServicePoller handles this entry. Empty string is
	// treated as "qbittorrent" for backward compatibility with pre-v1.3.0
	// configs. Known values: "qbittorrent", "sabnzbd", "dispatcharr".
	Type string `json:"type,omitempty"`

	// APIKey is used by services that authenticate with a static key (SAB).
	// It is omitted from JSON when empty so existing qBit-only configs stay
	// byte-identical when re-serialised.
	APIKey string `json:"apiKey,omitempty"`

	// Username / Password are used by services with no API key mechanism
	// (Dispatcharr v0.x authenticates via POST /api/accounts/token/ using
	// admin credentials — see docs/multi-service-monitoring-plan.md for the
	// security implications of storing the Dispatcharr password in config).
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`

	// ShowDetails enables the service-specific detail view on the Traffic
	// tab (e.g. active Dispatcharr streams with channel names). Only used
	// by pollers that implement ServiceDetailer — ignored otherwise.
	ShowDetails bool `json:"showDetails,omitempty"`

	// AutoSyncForwardedPort makes vpn-gateway push the current PIA / Proton
	// dynamic forwarded port (read from /config/wireguard/forwarded_port)
	// into this qBittorrent instance's listening port via setPreferences.
	// Replaces external sidecars like claabs/qbittorrent-port-forward-file.
	//
	// At most one PortMapping may have this true at a time — PIA / Proton
	// hand out one external port per tunnel, and two qBits trying to bind
	// the same incoming port would collide. Validated in ValidateConfig.
	// qBittorrent type only; ignored on other types.
	AutoSyncForwardedPort bool `json:"autoSyncForwardedPort,omitempty"`
}

// Current on-disk schema epoch for .traffic-ui.json. Bump when adding or
// removing fields; read-side is forward-compatible (Go unmarshal ignores
// unknown fields), so older containers can read newer files. v2 = auth
// fields added (v1.4.0 security hardening).
const CurrentConfigVersion = 2

type Config struct {
	Enabled         bool           `json:"enabled"`
	ScheduleEnabled bool           `json:"scheduleEnabled"`
	DefaultDown     int            `json:"defaultDown"`
	DefaultUp       int            `json:"defaultUp"`
	BurstMs         int            `json:"burstMs"`
	LogChanges      bool           `json:"logChanges"`
	ConfigVersion   int            `json:"configVersion"`
	Rules           []ScheduleRule `json:"rules"`
	Ports           []PortMapping  `json:"ports,omitempty"`

	// Authentication — matches Radarr/Sonarr Security panel model, mirrored
	// across our Go containers (Clonarr v2.0.6, Constat v0.9.17). Credentials
	// (bcrypt password hash, API key) live in /config/auth.json, NOT here, so
	// .traffic-ui.json can be shared/exported without leaking secrets.
	Authentication         string `json:"authentication,omitempty"`         // "forms" (default) | "basic" | "none"
	AuthenticationRequired string `json:"authenticationRequired,omitempty"` // "enabled" | "disabled_for_local_addresses" (default)
	TrustedProxies         string `json:"trustedProxies,omitempty"`         // comma-separated IPs — reverse-proxy deployments
	TrustedNetworks        string `json:"trustedNetworks,omitempty"`        // comma-separated IPs/CIDRs for local-bypass; empty = Radarr-parity default
	SessionTTLDays         int    `json:"sessionTtlDays,omitempty"`         // default 30

	// Dashboard display preferences — purely visual, no effect on traffic
	// shaping or auth. Kept in the UI config so changes don't require a
	// container restart and are visible immediately to the user.
	VPNIPDisplay          string `json:"vpnIpDisplay,omitempty"`          // "" (= "server") | "server" | "tunnel" | "both" | "off"
	ForwardedPortsDisplay string `json:"forwardedPortsDisplay,omitempty"` // "" (= "off") | "off" | "auto" | "all"
}

const uiConfigPath = "/config/.traffic-ui.json"

// ValidateConfig checks for invalid or dangerous values.
// Auth fields are validated here in addition to auth.ValidateConfig so that
// bad values get a 400 to the caller rather than a silent failure during
// the live auth-store reload.
func ValidateConfig(cfg *Config) error {
	if cfg.DefaultDown < 0 || cfg.DefaultUp < 0 || cfg.DefaultDown > 10000 || cfg.DefaultUp > 10000 {
		return fmt.Errorf("rates must be 0-10000 MB/s")
	}
	if cfg.BurstMs < 100 || cfg.BurstMs > 5000 {
		return fmt.Errorf("burst must be between 100 and 5000 ms")
	}
	if cfg.ConfigVersion < CurrentConfigVersion {
		cfg.ConfigVersion = CurrentConfigVersion
	}
	for i, rule := range cfg.Rules {
		if !validTime.MatchString(rule.Time) {
			return fmt.Errorf("rule %d: invalid time %q (expected HH:MM, 00-23)", i+1, rule.Time)
		}
		if rule.Days != "" && !validDays.MatchString(rule.Days) {
			return fmt.Errorf("rule %d: invalid days %q (use day names: mon,tue,wed,thu,fri,sat,sun)", i+1, rule.Days)
		}
		if rule.Down < 0 || rule.Up < 0 || rule.Down > 10000 || rule.Up > 10000 {
			return fmt.Errorf("rule %d: rates must be 0-10000 MB/s", i+1)
		}
	}
	seenPorts := make(map[int]bool)
	for i, pm := range cfg.Ports {
		if pm.Port < 1 || pm.Port > 65535 {
			return fmt.Errorf("port %d: must be 1-65535", i+1)
		}
		if pm.Name == "" {
			return fmt.Errorf("port %d: name is required", i+1)
		}
		if seenPorts[pm.Port] {
			return fmt.Errorf("port %d: duplicate port %d", i+1, pm.Port)
		}
		seenPorts[pm.Port] = true
		// Reject the credential mask sentinel reaching persistence. The PUT
		// handler's merge loop replaces the mask for existing ports, but a
		// freshly-added port has no prior entry to merge against — without
		// this guard the literal "********" would be stored as the password
		// and silently lock the user out of their qBit/Dispatcharr instance.
		if pm.Password == credentialMask {
			return fmt.Errorf("port %d (%s): password must be entered, not the masked placeholder", i+1, pm.Name)
		}
		if pm.APIKey == credentialMask {
			return fmt.Errorf("port %d (%s): API key must be entered, not the masked placeholder", i+1, pm.Name)
		}
	}

	// At most one PortMapping may auto-sync the dynamic forwarded port.
	// PIA / Proton hand out one external port per tunnel; two qBits trying
	// to bind that same incoming port would collide. Catch this at save
	// time so the UI gets a clear 400 instead of silent runtime fights.
	autoSyncCount := 0
	autoSyncFirst := ""
	for _, pm := range cfg.Ports {
		if !pm.AutoSyncForwardedPort {
			continue
		}
		if pm.Type != "" && pm.Type != "qbittorrent" {
			return fmt.Errorf("port %d (%s): autoSyncForwardedPort is only valid for type=qbittorrent", pm.Port, pm.Name)
		}
		autoSyncCount++
		if autoSyncFirst == "" {
			autoSyncFirst = pm.Name
		}
	}
	if autoSyncCount > 1 {
		return fmt.Errorf("only one qBit instance may have autoSyncForwardedPort enabled (currently %d enabled, first: %s) — PIA/Proton give a single dynamic port per tunnel", autoSyncCount, autoSyncFirst)
	}

	// Auth fields — validate the shape so a bad UI submission is rejected
	// with 400 before it hits the disk, instead of failing silently during
	// the live auth-store reload.
	switch cfg.Authentication {
	case "", "forms", "basic", "none":
	default:
		return fmt.Errorf("authentication: %q (expected forms | basic | none)", cfg.Authentication)
	}
	switch cfg.AuthenticationRequired {
	case "", "enabled", "disabled_for_local_addresses":
	default:
		return fmt.Errorf("authenticationRequired: %q (expected enabled | disabled_for_local_addresses)", cfg.AuthenticationRequired)
	}
	if cfg.SessionTTLDays < 0 || cfg.SessionTTLDays > 365 {
		return fmt.Errorf("sessionTtlDays must be 0-365 (0 = use default)")
	}
	switch cfg.VPNIPDisplay {
	case "", "server", "tunnel", "both", "off":
	default:
		return fmt.Errorf("vpnIpDisplay: %q (expected off | server | tunnel | both)", cfg.VPNIPDisplay)
	}
	switch cfg.ForwardedPortsDisplay {
	case "", "off", "auto", "all":
	default:
		return fmt.Errorf("forwardedPortsDisplay: %q (expected off | auto | all)", cfg.ForwardedPortsDisplay)
	}
	if cfg.TrustedNetworks != "" {
		if _, err := netsec.ParseTrustedNetworks(cfg.TrustedNetworks); err != nil {
			return fmt.Errorf("trustedNetworks: %v", err)
		}
	}
	if cfg.TrustedProxies != "" {
		if _, err := netsec.ParseTrustedProxies(cfg.TrustedProxies); err != nil {
			return fmt.Errorf("trustedProxies: %v", err)
		}
	}
	return nil
}

type NftCounter struct {
	RateKbytes int64  `json:"rateKbytes"`
	RateMB     int    `json:"rateMB"`
	Packets    int64  `json:"packets"`
	Bytes      int64  `json:"bytes"`
	Comment    string `json:"comment"`
	Active     bool   `json:"active"`
}

// loadConfig reads config from whichever source is newer:
// - .traffic-ui.json (written by web UI)
// - traffic.conf (written manually or by nft-apply)
// If only one exists, use that. If both exist, use the newer one.
func loadConfig(confPath string) (*Config, error) {
	jsonInfo, jsonErr := os.Stat(uiConfigPath)
	confInfo, confErr := os.Stat(confPath)

	haveJSON := jsonErr == nil
	haveConf := confErr == nil

	// If both exist, use whichever was modified more recently
	if haveJSON && haveConf {
		if confInfo.ModTime().After(jsonInfo.ModTime()) {
			// traffic.conf was edited manually — re-parse it, but preserve
			// JSON-only fields (ports + auth). Bash config deliberately
			// never carries auth fields; losing them here would silently
			// reset the admin's Security-panel settings to defaults on the
			// next initAuth or handlePutConfig read.
			cfg, err := parseBashConfig(confPath)
			if err != nil {
				return nil, err
			}
			if data, err := os.ReadFile(uiConfigPath); err == nil {
				var jsonCfg Config
				if err := json.Unmarshal(data, &jsonCfg); err == nil {
					cfg.Ports = jsonCfg.Ports
					cfg.Authentication = jsonCfg.Authentication
					cfg.AuthenticationRequired = jsonCfg.AuthenticationRequired
					cfg.TrustedNetworks = jsonCfg.TrustedNetworks
					cfg.TrustedProxies = jsonCfg.TrustedProxies
					cfg.SessionTTLDays = jsonCfg.SessionTTLDays
					cfg.VPNIPDisplay = jsonCfg.VPNIPDisplay
					cfg.ForwardedPortsDisplay = jsonCfg.ForwardedPortsDisplay
				}
			}
			return cfg, nil
		}
		// JSON is newer or same age — use it
		data, err := os.ReadFile(uiConfigPath)
		if err == nil {
			var cfg Config
			if err := json.Unmarshal(data, &cfg); err == nil {
				return &cfg, nil
			}
		}
	}

	// Only JSON exists
	if haveJSON {
		data, err := os.ReadFile(uiConfigPath)
		if err == nil {
			var cfg Config
			if err := json.Unmarshal(data, &cfg); err == nil {
				return &cfg, nil
			}
		}
	}

	// Fallback: parse bash config
	return parseBashConfig(confPath)
}

// parseBashConfig reads traffic.conf (bash key=value format)
func parseBashConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &Config{
		Enabled:       true,
		BurstMs:       500,
		LogChanges:    true,
		ConfigVersion: 1,
	}

	vars := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, "\"'")
			vars[key] = val
		}
	}

	if v, ok := vars["ENABLED"]; ok {
		cfg.Enabled = v == "true"
	}
	if v, ok := vars["SCHEDULE_ENABLED"]; ok {
		cfg.ScheduleEnabled = v == "true"
	}
	parseIntWarn := func(key, val string) int {
		n, err := strconv.Atoi(val)
		if err != nil {
			log.Printf("Warning: invalid value for %s=%q, using 0", key, val)
		}
		return n
	}

	if v, ok := vars["DEFAULT_DOWN"]; ok {
		cfg.DefaultDown = parseIntWarn("DEFAULT_DOWN", v)
	}
	if v, ok := vars["DEFAULT_UP"]; ok {
		cfg.DefaultUp = parseIntWarn("DEFAULT_UP", v)
	}
	if v, ok := vars["BURST_MS"]; ok {
		cfg.BurstMs = parseIntWarn("BURST_MS", v)
	}
	if v, ok := vars["LOG_CHANGES"]; ok {
		cfg.LogChanges = v == "true"
	}
	if v, ok := vars["CONFIG_VERSION"]; ok {
		cfg.ConfigVersion = parseIntWarn("CONFIG_VERSION", v)
	}

	for n := 1; n <= 50; n++ {
		timeKey := fmt.Sprintf("SCHEDULE_%d_TIME", n)
		if timeVal, ok := vars[timeKey]; ok {
			if !validTime.MatchString(timeVal) {
				log.Printf("Warning: invalid time %s=%q, skipping rule", timeKey, timeVal)
				continue
			}
			rule := ScheduleRule{Time: timeVal}
			if v, ok := vars[fmt.Sprintf("SCHEDULE_%d_DOWN", n)]; ok {
				rule.Down = parseIntWarn(fmt.Sprintf("SCHEDULE_%d_DOWN", n), v)
			}
			if v, ok := vars[fmt.Sprintf("SCHEDULE_%d_UP", n)]; ok {
				rule.Up = parseIntWarn(fmt.Sprintf("SCHEDULE_%d_UP", n), v)
			}
			if v, ok := vars[fmt.Sprintf("SCHEDULE_%d_DAYS", n)]; ok {
				rule.Days = v
			}
			cfg.Rules = append(cfg.Rules, rule)
		}
	}

	return cfg, nil
}

// saveConfig writes both the UI JSON (authoritative) and the bash config
// (consumed by nft-apply/svc-traffic).
//
// .traffic-ui.json is 0600 because it holds secrets (qBit passwords, SAB
// API keys, Dispatcharr admin creds). traffic.conf is 0644 — read by the
// root-owned svc-traffic service and the config generator never writes
// secrets into it.
func saveConfig(confPath string, cfg *Config) error {
	// 1. Save UI JSON (full model, contains secrets) — atomic + 0600.
	jsonData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal UI config: %w", err)
	}
	if err := atomicWriteFile(uiConfigPath, jsonData, 0600); err != nil {
		return fmt.Errorf("write UI config: %w", err)
	}

	// 2. Generate bash config (secret-free) — atomic + 0644.
	bash := generateBashConfig(cfg)
	if err := atomicWriteFile(confPath, []byte(bash), 0644); err != nil {
		return fmt.Errorf("write bash config: %w", err)
	}

	return nil
}

// generateBashConfig creates the traffic.conf content from the UI config.
// Rules with timeTo are expanded into two bash rules (start + end).
func generateBashConfig(cfg *Config) string {
	var b strings.Builder

	b.WriteString("# ============================================================================\n")
	b.WriteString("# traffic.conf — VPN Gateway Bandwidth Manager (nftables)\n")
	b.WriteString("# ============================================================================\n")
	b.WriteString("# Auto-generated by web UI. Edit via http://localhost:8090 or delete\n")
	b.WriteString("# /config/.traffic-ui.json to revert to manual config editing.\n")
	b.WriteString("# ============================================================================\n\n")

	b.WriteString(fmt.Sprintf("CONFIG_VERSION=%d\n\n", cfg.ConfigVersion))

	if cfg.Enabled {
		b.WriteString("ENABLED=true\n")
	} else {
		b.WriteString("ENABLED=false\n")
	}

	b.WriteString(fmt.Sprintf("\nDEFAULT_DOWN=%d\n", cfg.DefaultDown))
	b.WriteString(fmt.Sprintf("DEFAULT_UP=%d\n", cfg.DefaultUp))

	b.WriteString("\n")
	if cfg.ScheduleEnabled {
		b.WriteString("SCHEDULE_ENABLED=true\n")
	} else {
		b.WriteString("SCHEDULE_ENABLED=false\n")
	}

	sort.Slice(cfg.Rules, func(i, j int) bool {
		return cfg.Rules[i].Time < cfg.Rules[j].Time
	})

	for n, rule := range cfg.Rules {
		b.WriteString(fmt.Sprintf("\nSCHEDULE_%d_TIME=\"%s\"\n", n+1, rule.Time))
		b.WriteString(fmt.Sprintf("SCHEDULE_%d_DOWN=%d\n", n+1, rule.Down))
		b.WriteString(fmt.Sprintf("SCHEDULE_%d_UP=%d\n", n+1, rule.Up))
		if rule.Days != "" {
			b.WriteString(fmt.Sprintf("SCHEDULE_%d_DAYS=\"%s\"\n", n+1, rule.Days))
		}
	}

	b.WriteString(fmt.Sprintf("\nBURST_MS=%d\n", cfg.BurstMs))
	if cfg.LogChanges {
		b.WriteString("LOG_CHANGES=true\n")
	} else {
		b.WriteString("LOG_CHANGES=false\n")
	}

	return b.String()
}

// ActiveRuleInfo describes which rule is currently in effect
type ActiveRuleInfo struct {
	Source    string `json:"source"`              // "rule", "default", "disabled"
	Index     int    `json:"index"`               // 1-based rule index (0 if default/disabled)
	Time      string `json:"time"`                // rule time (empty if default)
	Days      string `json:"days"`                // rule days (empty if default)
	Down      int    `json:"down"`
	Up        int    `json:"up"`
	NextIndex int    `json:"nextIndex,omitempty"` // 1-based index of next rule (0 if none)
	NextTime  string `json:"nextTime,omitempty"`  // time of next rule
}

// resolveActiveRule determines which schedule rule is currently active.
// Mirrors nft-apply's resolve_current_rates logic.
func resolveActiveRule(cfg *Config) ActiveRuleInfo {
	if !cfg.Enabled {
		return ActiveRuleInfo{Source: "disabled"}
	}
	if !cfg.ScheduleEnabled || len(cfg.Rules) == 0 {
		return ActiveRuleInfo{Source: "default", Down: cfg.DefaultDown, Up: cfg.DefaultUp}
	}

	now := time.Now()
	nowMins := now.Hour()*60 + now.Minute()
	todayDow := strings.ToLower(now.Weekday().String()[:3])

	allDays := []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

	// Check if a day matches a spec
	dayMatches := func(day, spec string) bool {
		if spec == "" {
			return true
		}
		// Range
		if strings.Contains(spec, "-") {
			parts := strings.SplitN(spec, "-", 2)
			start, end := strings.ToLower(parts[0]), strings.ToLower(parts[1])
			inRange := false
			for i := 0; i < 14; i++ {
				d := allDays[i%7]
				if d == start {
					inRange = true
				}
				if inRange && d == day {
					return true
				}
				if d == end && inRange {
					break
				}
			}
			return false
		}
		// Comma list
		for _, d := range strings.Split(spec, ",") {
			if strings.ToLower(strings.TrimSpace(d)) == day {
				return true
			}
		}
		return false
	}

	// Find best matching rule: latest time that has passed today on matching day
	type candidate struct {
		ruleIdx int
		rule    ScheduleRule
		mins    int
	}

	// parseTimeMins safely parses "HH:MM" into minutes since midnight.
	// Returns -1 if the format is invalid.
	parseTimeMins := func(t string) int {
		parts := strings.Split(t, ":")
		if len(parts) != 2 {
			return -1
		}
		h, err := strconv.Atoi(parts[0])
		if err != nil {
			return -1
		}
		m, err := strconv.Atoi(parts[1])
		if err != nil {
			return -1
		}
		return h*60 + m
	}

	findBest := func(dow string, maxMins int) *candidate {
		var best *candidate
		for i, r := range cfg.Rules {
			if !dayMatches(dow, r.Days) {
				continue
			}
			mins := parseTimeMins(r.Time)
			if mins < 0 {
				continue
			}
			if mins <= maxMins {
				if best == nil || mins > best.mins {
					best = &candidate{ruleIdx: i, rule: r, mins: mins}
				}
			}
		}
		return best
	}

	// Find earliest rule on a given day (for finding next-day rules)
	findEarliest := func(dow string) *candidate {
		var earliest *candidate
		for i, r := range cfg.Rules {
			if !dayMatches(dow, r.Days) {
				continue
			}
			mins := parseTimeMins(r.Time)
			if mins < 0 {
				continue
			}
			if earliest == nil || mins < earliest.mins {
				earliest = &candidate{ruleIdx: i, rule: r, mins: mins}
			}
		}
		return earliest
	}

	// Find next rule after a given time on a given day
	findNext := func(dow string, afterMins int) *candidate {
		var next *candidate
		for i, r := range cfg.Rules {
			if !dayMatches(dow, r.Days) {
				continue
			}
			mins := parseTimeMins(r.Time)
			if mins < 0 {
				continue
			}
			if mins > afterMins {
				if next == nil || mins < next.mins {
					next = &candidate{ruleIdx: i, rule: r, mins: mins}
				}
			}
		}
		return next
	}

	// Check today first
	best := findBest(todayDow, nowMins)

	// If nothing today, look back up to 7 days
	if best == nil {
		for back := 1; back <= 7; back++ {
			prevDay := now.AddDate(0, 0, -back)
			dow := strings.ToLower(prevDay.Weekday().String()[:3])
			best = findBest(dow, 1440)
			if best != nil {
				break
			}
		}
	}

	if best != nil {
		info := ActiveRuleInfo{
			Source: "rule",
			Index:  best.ruleIdx + 1,
			Time:   best.rule.Time,
			Days:   best.rule.Days,
			Down:   best.rule.Down,
			Up:     best.rule.Up,
		}

		// Find next rule: first check later today, then upcoming days
		nextToday := findNext(todayDow, nowMins)
		if nextToday != nil {
			info.NextIndex = nextToday.ruleIdx + 1
			info.NextTime = nextToday.rule.Time
		} else {
			// Check tomorrow and forward (up to 7 days)
			for fwd := 1; fwd <= 7; fwd++ {
				futureDay := now.AddDate(0, 0, fwd)
				dow := strings.ToLower(futureDay.Weekday().String()[:3])
				earliest := findEarliest(dow)
				if earliest != nil {
					info.NextIndex = earliest.ruleIdx + 1
					info.NextTime = earliest.rule.Time
					break
				}
			}
		}

		return info
	}

	return ActiveRuleInfo{Source: "default", Down: cfg.DefaultDown, Up: cfg.DefaultUp}
}

// getNftCounters reads current nft rate limit rules and their counters
func getNftCounters() (up NftCounter, down NftCounter) {
	out, err := exec.Command("nft", "-a", "list", "table", "inet", "hotio").Output()
	if err != nil {
		return
	}

	lines := strings.Split(string(out), "\n")
	rateRe := regexp.MustCompile(`limit rate over (\d+) (\w+)/second`)
	counterRe := regexp.MustCompile(`counter packets (\d+) bytes (\d+)`)

	for _, line := range lines {
		if strings.Contains(line, "traffic-limit-up") {
			up.Active = true
			up.Comment = "traffic-limit-up"
			if m := rateRe.FindStringSubmatch(line); m != nil {
				rate, _ := strconv.ParseInt(m[1], 10, 64)
				unit := m[2]
				up.RateKbytes = rate
				if unit == "mbytes" {
					up.RateMB = int(rate)
					up.RateKbytes = rate * 1024
				} else {
					up.RateMB = int(rate / 1024)
				}
			}
			if m := counterRe.FindStringSubmatch(line); m != nil {
				up.Packets, _ = strconv.ParseInt(m[1], 10, 64)
				up.Bytes, _ = strconv.ParseInt(m[2], 10, 64)
			}
		}
		if strings.Contains(line, "traffic-limit-down") {
			down.Active = true
			down.Comment = "traffic-limit-down"
			if m := rateRe.FindStringSubmatch(line); m != nil {
				rate, _ := strconv.ParseInt(m[1], 10, 64)
				unit := m[2]
				down.RateKbytes = rate
				if unit == "mbytes" {
					down.RateMB = int(rate)
					down.RateKbytes = rate * 1024
				} else {
					down.RateMB = int(rate / 1024)
				}
			}
			if m := counterRe.FindStringSubmatch(line); m != nil {
				down.Packets, _ = strconv.ParseInt(m[1], 10, 64)
				down.Bytes, _ = strconv.ParseInt(m[2], 10, 64)
			}
		}
	}

	return
}
