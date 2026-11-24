package mihomo

import (
	"context"
	"net/http"
	"time"
)

// TrafficConnection is the subset used for country-level local flight maps.
// Hostnames, process paths, source addresses and rule payloads are never decoded.
type TrafficConnection struct {
	ID       string    `json:"id"`
	Start    time.Time `json:"start"`
	Chains   []string  `json:"chains"`
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
	Metadata struct {
		Destination     string   `json:"destinationIP"`
		TargetCountries []string `json:"destinationGeoIP"`
		Peer            string   `json:"remoteDestination"`
	} `json:"metadata"`
}

func (c *Client) ActiveTraffic(ctx context.Context) ([]TrafficConnection, error) {
	var data struct {
		Connections []TrafficConnection `json:"connections"`
	}
	if err := c.json(ctx, http.MethodGet, "/connections", nil, &data); err != nil {
		return nil, err
	}
	return data.Connections, nil
}
