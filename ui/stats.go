package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PortRate holds per-port rx/tx for a single point
type PortRate struct {
	Rx float64 `json:"r"`
	Tx float64 `json:"s"`
}

// TrafficPoint is a single data point in the ring buffer
type TrafficPoint struct {
	Time   int64              `json:"t"`           // Unix timestamp
	RxRate float64            `json:"r"`           // Total download bytes/sec
	TxRate float64            `json:"s"`           // Total upload bytes/sec
	Ports  map[int]PortRate   `json:"p,omitempty"` // Per-port rates (key=port number)
}

// PortStats holds per-port traffic data
type PortStats struct {
	Port    int     `json:"port"`
	Name    string  `json:"name"`
	RxRate  float64 `json:"rxRate"`  // bytes/sec
	TxRate  float64 `json:"txRate"`  // bytes/sec
	RxBytes uint64  `json:"rxBytes"` // total bytes
	TxBytes uint64  `json:"txBytes"` // total bytes
}

// TrafficStats is the live snapshot sent via SSE
type TrafficStats struct {
	RxRate    float64     `json:"rxRate"`    // Current download bytes/sec
	TxRate    float64     `json:"txRate"`    // Current upload bytes/sec
	RxBytes   uint64      `json:"rxBytes"`   // Total received since boot
	TxBytes   uint64      `json:"txBytes"`   // Total sent since boot
	MaxRx24h  float64     `json:"maxRx24h"`  // Peak download last 24h
	MaxTx24h  float64     `json:"maxTx24h"`  // Peak upload last 24h
	AvgRx24h  float64     `json:"avgRx24h"`  // Avg download last 24h
	AvgTx24h  float64     `json:"avgTx24h"`  // Avg upload last 24h
	TotalRx   uint64      `json:"totalRx"`   // Cumulative total download
	TotalTx   uint64      `json:"totalTx"`   // Cumulative total upload
	Vol24hRx  uint64      `json:"vol24hRx"`  // Download volume last 24h
	Vol24hTx  uint64      `json:"vol24hTx"`  // Upload volume last 24h
	Ports     []PortStats `json:"ports"`     // Per-port stats
}

// PersistedTotals tracks cumulative VPN-wide bytes across restarts
type PersistedTotals struct {
	RxBytes uint64 `json:"rxBytes"`
	TxBytes uint64 `json:"txBytes"`
}

// DailyVolume tracks bytes transferred per day
type DailyVolume struct {
	Date    string            `json:"date"`    // YYYY-MM-DD
	RxBytes uint64            `json:"rxBytes"` // total download
	TxBytes uint64            `json:"txBytes"` // total upload
	Ports   map[int]PortBytes `json:"ports,omitempty"`
}

type PortBytes struct {
	RxBytes uint64 `json:"rxBytes"`
	TxBytes uint64 `json:"txBytes"`
}

// PersistedPortTotal tracks cumulative per-port bytes across restarts
type PersistedPortTotal struct {
	Port    int    `json:"port"`
	Name    string `json:"name"`
	RxBytes uint64 `json:"rxBytes"`
	TxBytes uint64 `json:"txBytes"`
}

// PersistedTraffic is saved to disk
type PersistedTraffic struct {
	Version    int                  `json:"version"`
	History    []TrafficPoint       `json:"history"`
	PortTotals []PersistedPortTotal `json:"portTotals,omitempty"`
	Totals     PersistedTotals      `json:"totals"`
	DailyVols  []DailyVolume        `json:"dailyVols,omitempty"`
}

const (
	sampleInterval = 3 * time.Second
	ringSize       = 86400 // 72h at 3s intervals
	persistPath    = "/config/.traffic-stats.json"
	persistInterval = 5 * time.Minute
)

// portCounter tracks per-service traffic via qBit API
type portCounter struct {
	port    int
	name    string
	totalRx uint64 // cumulative across restarts (from persisted + API session data)
	totalTx uint64
	rxRate  float64 // live speed from API
	txRate  float64
	// Track session offsets for cumulative calculation
	baseRx uint64 // persisted total at start of this session
	baseTx uint64
}

// TrafficCollector samples wg0 interface stats and maintains history
type TrafficCollector struct {
	mu      sync.RWMutex
	iface   string
	configPath string
	history []TrafficPoint
	head    int // next write position in ring buffer
	count   int // number of valid entries
	prevRx   uint64
	prevTx   uint64
	prevTime time.Time
	latest    TrafficStats
	totalRx   uint64 // cumulative total across restarts
	totalTx   uint64
	baseRx    uint64
	baseTx    uint64
	dailyVols []DailyVolume // last 30 days
	today     string        // current day YYYY-MM-DD

	// Per-port counters
	portCounters []portCounter

	// SSE subscribers
	subMu   sync.Mutex
	subs    map[chan TrafficStats]struct{}
}

func NewTrafficCollector(iface, configPath string) *TrafficCollector {
	tc := &TrafficCollector{
		iface:      iface,
		configPath: configPath,
		history:    make([]TrafficPoint, ringSize),
		subs:       make(map[chan TrafficStats]struct{}),
	}
	tc.loadFromDisk()
	return tc
}

func (tc *TrafficCollector) Run(ctx context.Context) {
	// Wait for interface to appear
	for {
		if _, _, err := readInterfaceBytes(tc.iface); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	// Initialize counters
	rx, tx, _ := readInterfaceBytes(tc.iface)
	tc.prevRx = rx
	tc.prevTx = tx
	tc.prevTime = time.Now()

	log.Printf("Traffic collector started on %s", tc.iface)

	// Initial port counter setup
	tc.syncPortCounters()

	sampleTick := time.NewTicker(sampleInterval)
	persistTick := time.NewTicker(persistInterval)
	portSyncTick := time.NewTicker(30 * time.Second) // re-sync port config periodically
	defer sampleTick.Stop()
	defer persistTick.Stop()
	defer portSyncTick.Stop()

	for {
		select {
		case <-ctx.Done():
			tc.saveToDisk()
			return
		case <-sampleTick.C:
			tc.sample()
			tc.broadcast()
		case <-persistTick.C:
			tc.saveToDisk()
		case <-portSyncTick.C:
			tc.syncPortCounters()
		}
	}
}

func (tc *TrafficCollector) sample() {
	rx, tx, err := readInterfaceBytes(tc.iface)
	now := time.Now()
	elapsed := now.Sub(tc.prevTime).Seconds()

	tc.mu.Lock()

	if err != nil || elapsed <= 0 {
		tc.prevTime = now
		tc.mu.Unlock()
		return
	}

	// Handle counter reset (VPN reconnect)
	var rxRate, txRate float64
	var rxDelta, txDelta uint64
	if rx >= tc.prevRx {
		rxDelta = rx - tc.prevRx
		rxRate = float64(rxDelta) / elapsed
		tc.totalRx += rxDelta
	}
	if tx >= tc.prevTx {
		txDelta = tx - tc.prevTx
		txRate = float64(txDelta) / elapsed
		tc.totalTx += txDelta
	}

	// Accumulate daily volume
	today := now.Format("2006-01-02")
	if tc.today != today {
		tc.today = today
		tc.dailyVols = append(tc.dailyVols, DailyVolume{Date: today, Ports: make(map[int]PortBytes)})
		// Keep last 365 days
		if len(tc.dailyVols) > 365 {
			tc.dailyVols = tc.dailyVols[len(tc.dailyVols)-365:]
		}
	}
	if len(tc.dailyVols) > 0 {
		d := &tc.dailyVols[len(tc.dailyVols)-1]
		d.RxBytes += rxDelta
		d.TxBytes += txDelta
	}

	tc.prevRx = rx
	tc.prevTx = tx
	tc.prevTime = now

	// Store in ring buffer (port data added after pollPortStats below)
	pt := TrafficPoint{
		Time:   now.Unix(),
		RxRate: rxRate,
		TxRate: txRate,
	}
	tc.history[tc.head] = pt
	currentHead := tc.head
	tc.head = (tc.head + 1) % ringSize
	if tc.count < ringSize {
		tc.count++
	}

	// Update latest stats
	tc.latest.RxRate = rxRate
	tc.latest.TxRate = txRate
	tc.latest.RxBytes = rx
	tc.latest.TxBytes = tx
	tc.latest.TotalRx = tc.totalRx
	tc.latest.TotalTx = tc.totalTx

	// Compute 24h peak and average
	daysamples := 28800 // 24h / 3s
	if daysamples > tc.count {
		daysamples = tc.count
	}
	var maxRx, maxTx, sumRx, sumTx float64
	for i := 0; i < daysamples; i++ {
		idx := (tc.head - 1 - i + ringSize) % ringSize
		p := tc.history[idx]
		if p.RxRate > maxRx {
			maxRx = p.RxRate
		}
		if p.TxRate > maxTx {
			maxTx = p.TxRate
		}
		sumRx += p.RxRate
		sumTx += p.TxRate
	}
	tc.latest.MaxRx24h = maxRx
	tc.latest.MaxTx24h = maxTx
	if daysamples > 0 {
		tc.latest.AvgRx24h = sumRx / float64(daysamples)
		tc.latest.AvgTx24h = sumTx / float64(daysamples)
	}
	// 24h volume = sum of rates * sample interval
	tc.latest.Vol24hRx = uint64(sumRx * sampleInterval.Seconds())
	tc.latest.Vol24hTx = uint64(sumTx * sampleInterval.Seconds())
	tc.mu.Unlock()

	// Poll qBit API for per-service stats
	portStats := tc.pollPortStats()

	tc.mu.Lock()
	tc.latest.Ports = portStats
	// Accumulate per-port daily volume from rates
	if len(portStats) > 0 && len(tc.dailyVols) > 0 {
		d := &tc.dailyVols[len(tc.dailyVols)-1]
		if d.Ports == nil {
			d.Ports = make(map[int]PortBytes)
		}
		for _, ps := range portStats {
			pb := d.Ports[ps.Port]
			pb.RxBytes += uint64(ps.RxRate * sampleInterval.Seconds())
			pb.TxBytes += uint64(ps.TxRate * sampleInterval.Seconds())
			d.Ports[ps.Port] = pb
		}
	}
	// Add per-port rates to the current history point
	if len(portStats) > 0 {
		portRates := make(map[int]PortRate, len(portStats))
		for _, ps := range portStats {
			portRates[ps.Port] = PortRate{Rx: ps.RxRate, Tx: ps.TxRate}
		}
		tc.history[currentHead].Ports = portRates
	}
	tc.mu.Unlock()
}

func (tc *TrafficCollector) broadcast() {
	tc.mu.RLock()
	stats := tc.latest
	tc.mu.RUnlock()

	tc.subMu.Lock()
	for ch := range tc.subs {
		select {
		case ch <- stats:
		default:
			// Drop if subscriber is slow
		}
	}
	tc.subMu.Unlock()
}

func (tc *TrafficCollector) Subscribe() chan TrafficStats {
	ch := make(chan TrafficStats, 4)
	tc.subMu.Lock()
	tc.subs[ch] = struct{}{}
	tc.subMu.Unlock()
	return ch
}

func (tc *TrafficCollector) Unsubscribe(ch chan TrafficStats) {
	tc.subMu.Lock()
	delete(tc.subs, ch)
	tc.subMu.Unlock()
	close(ch)
}

// GetHistory returns downsampled history for a given period
func (tc *TrafficCollector) GetHistory(period string) []TrafficPoint {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	var maxSamples, targetPoints int
	switch period {
	case "6h":
		maxSamples = 7200
		targetPoints = 720
	case "24h":
		maxSamples = 28800
		targetPoints = 720
	case "72h":
		maxSamples = ringSize
		targetPoints = 720
	default: // 1h
		maxSamples = 1200
		targetPoints = 1200
	}

	if maxSamples > tc.count {
		maxSamples = tc.count
	}

	// Only downsample when we have more data than target
	stride := 1
	if maxSamples > targetPoints {
		stride = maxSamples / targetPoints
	}

	var points []TrafficPoint
	for i := maxSamples - 1; i >= 0; i -= stride {
		idx := (tc.head - 1 - i + ringSize) % ringSize
		p := tc.history[idx]
		if p.Time > 0 {
			points = append(points, p)
		}
	}
	// Always include the most recent point
	if len(points) > 0 {
		newest := tc.history[(tc.head-1+ringSize)%ringSize]
		if newest.Time > 0 && newest.Time != points[len(points)-1].Time {
			points = append(points, newest)
		}
	}
	return points
}

// GetLatest returns current stats snapshot
func (tc *TrafficCollector) GetLatest() TrafficStats {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.latest
}

func (tc *TrafficCollector) saveToDisk() {
	tc.mu.RLock()

	// Extract valid history entries
	var history []TrafficPoint
	for i := 0; i < tc.count; i++ {
		idx := (tc.head - tc.count + i + ringSize) % ringSize
		p := tc.history[idx]
		if p.Time > 0 {
			history = append(history, p)
		}
	}
	// Capture port totals
	var portTotals []PersistedPortTotal
	for _, pc := range tc.portCounters {
		if pc.totalRx > 0 || pc.totalTx > 0 {
			portTotals = append(portTotals, PersistedPortTotal{
				Port:    pc.port,
				Name:    pc.name,
				RxBytes: pc.totalRx,
				TxBytes: pc.totalTx,
			})
		}
	}

	// Deep copy daily volumes (maps are reference types)
	dailyVols := make([]DailyVolume, len(tc.dailyVols))
	for i, dv := range tc.dailyVols {
		dailyVols[i] = DailyVolume{
			Date: dv.Date, RxBytes: dv.RxBytes, TxBytes: dv.TxBytes,
		}
		if dv.Ports != nil {
			dailyVols[i].Ports = make(map[int]PortBytes, len(dv.Ports))
			for k, v := range dv.Ports {
				dailyVols[i].Ports[k] = v
			}
		}
	}

	totalRx := tc.totalRx
	totalTx := tc.totalTx
	tc.mu.RUnlock()

	data := PersistedTraffic{
		Version:    1,
		History:    history,
		PortTotals: portTotals,
		Totals:     PersistedTotals{RxBytes: totalRx, TxBytes: totalTx},
		DailyVols:  dailyVols,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("Failed to marshal traffic stats: %v", err)
		return
	}

	tmpPath := persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, jsonData, 0664); err != nil {
		log.Printf("Failed to write traffic stats: %v", err)
		return
	}
	if err := os.Rename(tmpPath, persistPath); err != nil {
		os.Remove(tmpPath)
		log.Printf("Failed to rename traffic stats: %v", err)
	}
}

func (tc *TrafficCollector) loadFromDisk() {
	data, err := os.ReadFile(persistPath)
	if err != nil {
		return
	}

	var persisted PersistedTraffic
	if err := json.Unmarshal(data, &persisted); err != nil {
		log.Printf("Failed to parse traffic stats: %v", err)
		return
	}

	// Restore ring buffer
	tc.mu.Lock()
	defer tc.mu.Unlock()

	n := len(persisted.History)
	if n > ringSize {
		persisted.History = persisted.History[n-ringSize:]
		n = ringSize
	}

	j := 0
	for _, p := range persisted.History {
		if p.Time > 0 && p.RxRate >= 0 && p.TxRate >= 0 {
			tc.history[j] = p
			j++
		}
	}
	n = j
	tc.head = n % ringSize
	tc.count = n

	// Restore VPN totals and daily volumes
	tc.totalRx = persisted.Totals.RxBytes
	tc.totalTx = persisted.Totals.TxBytes
	tc.dailyVols = persisted.DailyVols
	if len(tc.dailyVols) > 0 {
		tc.today = tc.dailyVols[len(tc.dailyVols)-1].Date
	}

	// Restore port totals
	if len(persisted.PortTotals) > 0 {
		tc.portCounters = make([]portCounter, len(persisted.PortTotals))
		for i, pt := range persisted.PortTotals {
			tc.portCounters[i] = portCounter{
				port:    pt.Port,
				name:    pt.Name,
				totalRx: pt.RxBytes,
				totalTx: pt.TxBytes,
			}
		}
	}

	log.Printf("Loaded %d traffic history points from disk", n)
}

var validIface = regexp.MustCompile(`^[a-zA-Z0-9\-]+$`)

// qBitTransferInfo is the response from qBit API /api/v2/transfer/info
type qBitTransferInfo struct {
	DlInfoSpeed int64 `json:"dl_info_speed"`
	UpInfoSpeed int64 `json:"up_info_speed"`
	DlInfoData  int64 `json:"dl_info_data"`  // session total downloaded
	UpInfoData  int64 `json:"up_info_data"`  // session total uploaded
}

var httpClient = &http.Client{Timeout: 3 * time.Second}

// syncPortCounters reads config and updates port counter list (no nft rules needed)
func (tc *TrafficCollector) syncPortCounters() {
	cfg, err := loadConfig(tc.configPath)
	if err != nil || len(cfg.Ports) == 0 {
		tc.mu.Lock()
		tc.portCounters = nil
		tc.mu.Unlock()
		return
	}

	// Check if config changed
	tc.mu.RLock()
	changed := len(tc.portCounters) != len(cfg.Ports)
	if !changed {
		for i, pc := range tc.portCounters {
			if pc.port != cfg.Ports[i].Port || pc.name != cfg.Ports[i].Name {
				changed = true
				break
			}
		}
	}
	tc.mu.RUnlock()

	if !changed {
		return
	}

	log.Printf("Port monitoring config changed — updating service list")

	tc.mu.Lock()
	oldCounters := tc.portCounters
	tc.portCounters = make([]portCounter, len(cfg.Ports))

	// Map old totals by port
	oldTotals := make(map[int]portCounter)
	for _, oc := range oldCounters {
		oldTotals[oc.port] = oc
	}

	for i, pm := range cfg.Ports {
		pc := portCounter{port: pm.Port, name: pm.Name}
		if old, ok := oldTotals[pm.Port]; ok {
			pc.totalRx = old.totalRx
			pc.totalTx = old.totalTx
			pc.baseRx = old.baseRx
			pc.baseTx = old.baseTx
		}
		tc.portCounters[i] = pc
	}
	tc.mu.Unlock()
}

// pollPortStats queries qBit API on each configured port for live speed + session totals.
// HTTP calls are done WITHOUT holding the mutex to avoid blocking other operations.
func (tc *TrafficCollector) pollPortStats() []PortStats {
	tc.mu.RLock()
	counters := make([]portCounter, len(tc.portCounters))
	copy(counters, tc.portCounters)
	tc.mu.RUnlock()

	if len(counters) == 0 {
		return nil
	}

	// Collect API responses without lock
	type apiResult struct {
		info qBitTransferInfo
		ok   bool
	}
	results := make([]apiResult, len(counters))

	for i, pc := range counters {
		url := fmt.Sprintf("http://localhost:%d/api/v2/transfer/info", pc.port)
		resp, err := httpClient.Get(url)
		if err != nil {
			continue
		}
		var info qBitTransferInfo
		err = json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
		if err == nil {
			results[i] = apiResult{info: info, ok: true}
		}
	}

	// Now update state under lock
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Guard against config change between copy and lock
	if len(tc.portCounters) != len(counters) {
		return nil
	}

	var stats []PortStats
	for i := range counters {
		pc := &tc.portCounters[i]

		if !results[i].ok {
			stats = append(stats, PortStats{
				Port: pc.port, Name: pc.name,
				RxBytes: pc.totalRx, TxBytes: pc.totalTx,
			})
			continue
		}

		info := results[i].info

		// Clamp negative values to 0
		if info.DlInfoData < 0 { info.DlInfoData = 0 }
		if info.UpInfoData < 0 { info.UpInfoData = 0 }

		sessionRx := uint64(info.DlInfoData)
		sessionTx := uint64(info.UpInfoData)

		// Detect qBit restart: session data dropped below what we've seen
		prevSessionRx := uint64(0)
		if pc.totalRx > pc.baseRx {
			prevSessionRx = pc.totalRx - pc.baseRx
		}
		prevSessionTx := uint64(0)
		if pc.totalTx > pc.baseTx {
			prevSessionTx = pc.totalTx - pc.baseTx
		}

		if sessionRx < prevSessionRx || sessionTx < prevSessionTx {
			// qBit restarted — accumulate previous total as new base
			pc.baseRx = pc.totalRx
			pc.baseTx = pc.totalTx
		} else if pc.baseRx == 0 && pc.baseTx == 0 && (pc.totalRx > 0 || pc.totalTx > 0) {
			// First poll after vpn-gateway restart with persisted totals
			pc.baseRx = pc.totalRx
			pc.baseTx = pc.totalTx
		}

		pc.totalRx = pc.baseRx + sessionRx
		pc.totalTx = pc.baseTx + sessionTx
		pc.rxRate = float64(info.DlInfoSpeed)
		pc.txRate = float64(info.UpInfoSpeed)

		stats = append(stats, PortStats{
			Port:    pc.port,
			Name:    pc.name,
			RxRate:  float64(info.DlInfoSpeed),
			TxRate:  float64(info.UpInfoSpeed),
			RxBytes: pc.totalRx,
			TxBytes: pc.totalTx,
		})
	}

	return stats
}

func readInterfaceBytes(iface string) (rx, tx uint64, err error) {
	if !validIface.MatchString(iface) {
		return 0, 0, fmt.Errorf("invalid interface name: %s", iface)
	}
	rxData, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", iface))
	if err != nil {
		return 0, 0, err
	}
	txData, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", iface))
	if err != nil {
		return 0, 0, err
	}

	rx, err = strconv.ParseUint(strings.TrimSpace(string(rxData)), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	tx, err = strconv.ParseUint(strings.TrimSpace(string(txData)), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return rx, tx, nil
}

// --- HTTP Handlers ---

// handleStatsSSE streams live traffic stats via SSE
func (app *App) handleStatsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := app.traffic.Subscribe()
	defer app.traffic.Unsubscribe(ch)

	// Send initial data immediately
	stats := app.traffic.GetLatest()
	data, _ := json.Marshal(stats)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case stats, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(stats)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleStatsHistory returns historical traffic data
func (app *App) handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "1h"
	}
	points := app.traffic.GetHistory(period)
	writeJSON(w, points)
}

// handleStatsLatest returns current stats snapshot
func (app *App) handleStatsLatest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, app.traffic.GetLatest())
}

// Reset clears all traffic statistics
func (tc *TrafficCollector) Reset() {
	tc.mu.Lock()
	tc.history = make([]TrafficPoint, ringSize)
	tc.head = 0
	tc.count = 0
	tc.totalRx = 0
	tc.totalTx = 0
	tc.dailyVols = nil
	tc.today = ""
	tc.latest = TrafficStats{}
	for i := range tc.portCounters {
		tc.portCounters[i].totalRx = 0
		tc.portCounters[i].totalTx = 0
		tc.portCounters[i].baseRx = 0
		tc.portCounters[i].baseTx = 0
	}
	tc.mu.Unlock()
	tc.saveToDisk()
	log.Println("Traffic statistics reset")
}

// handleStatsReset clears all statistics
func (app *App) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	app.traffic.Reset()
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleStatsDailyVolume returns daily volume data
func (app *App) handleStatsDailyVolume(w http.ResponseWriter, r *http.Request) {
	tc := app.traffic
	tc.mu.RLock()
	// Deep copy to avoid data race on Ports maps
	vols := make([]DailyVolume, len(tc.dailyVols))
	for i, dv := range tc.dailyVols {
		vols[i] = DailyVolume{
			Date: dv.Date, RxBytes: dv.RxBytes, TxBytes: dv.TxBytes,
		}
		if dv.Ports != nil {
			vols[i].Ports = make(map[int]PortBytes, len(dv.Ports))
			for k, v := range dv.Ports {
				vols[i].Ports[k] = v
			}
		}
	}
	tc.mu.RUnlock()
	writeJSON(w, vols)
}
