package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// qBitPoller implements ServicePoller for qBittorrent's WebUI API.
// This is the historical default — existing configs that have no Type
// field on their PortMapping will fall through to this poller via the
// empty-string key registered in init().
type qBitPoller struct{}

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

// Poll fetches the current transfer info from qBittorrent. No authentication
// is assumed — this matches the existing vpn-gateway behaviour where qBit
// runs with LocalHostAuth=false on localhost inside the shared network
// namespace. A non-nil error means the request failed; the caller will
// preserve the previously persisted counter values for this port.
func (q *qBitPoller) Poll(ctx context.Context, mapping PortMapping) (ServiceStats, error) {
	url := fmt.Sprintf("http://localhost:%d/api/v2/transfer/info", mapping.Port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ServiceStats{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return ServiceStats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ServiceStats{}, fmt.Errorf("qbit: unexpected status %d", resp.StatusCode)
	}
	var info qBitTransferInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
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

func init() {
	// Register under both the canonical type key and the empty-string key so
	// existing configs with no Type field on their PortMapping continue to
	// work as qBittorrent without any migration.
	p := &qBitPoller{}
	registerPoller(p, "qbittorrent", "")
}
