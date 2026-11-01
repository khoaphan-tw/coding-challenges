// Package driverclient is a driven adapter: it lists drivers from
// driver-service over HTTP (usecase.DriverDirectory), used only to rebuild
// the GEO index.
package driverclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ridehail/pkg/geo"
	"ridehail/services/dispatch/internal/usecase"
)

// Client calls driver-service's /internal/drivers.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

var _ usecase.DriverDirectory = (*Client)(nil)

// ListDrivers returns every driver with status, location and version.
func (c *Client) ListDrivers(ctx context.Context) ([]usecase.DriverSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/internal/drivers", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("driver-service returned %d", resp.StatusCode)
	}
	var drivers []struct {
		ID        string  `json:"driverId"`
		Status    string  `json:"status"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Version   int64   `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&drivers); err != nil {
		return nil, err
	}
	out := make([]usecase.DriverSnapshot, 0, len(drivers))
	for _, d := range drivers {
		out = append(out, usecase.DriverSnapshot{ID: d.ID, Status: d.Status,
			Location: geo.Point{Latitude: d.Latitude, Longitude: d.Longitude}, Version: d.Version})
	}
	return out, nil
}
