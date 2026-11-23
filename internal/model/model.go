// Package model defines controller data and measurements shared by the guard.
package model

import (
	"strconv"
	"strings"
	"time"
)

// QuoteName keeps emoji joined on screen while escaping quotes, controls and
// literal backslashes in a Mihomo name.
func QuoteName(name string) string {
	const joiner = "\u200d"
	parts := strings.Split(name, joiner)
	for i, part := range parts {
		quoted := strconv.Quote(part)
		parts[i] = quoted[1 : len(quoted)-1]
	}
	return `"` + strings.Join(parts, joiner) + `"`
}

type Proxy struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Now      string   `json:"now"`
	All      []string `json:"all"`
	Provider string   `json:"provider-name"`
	Dialer   string   `json:"dialer-proxy"`
}

type Sample struct {
	At        time.Time `json:"time"`
	Node      string    `json:"node"`
	Provider  string    `json:"provider,omitempty"`
	Scope     string    `json:"scope"` // front, or a chain name for a heartbeat
	Kind      string    `json:"kind"`  // sweep, quick, confirm, bench, heartbeat
	Mbps      float64   `json:"download_mbps"`
	Bytes     int64     `json:"bytes"`
	ElapsedMS int64     `json:"elapsed_ms"`
	LatencyMS int       `json:"probe_latency_ms"`
	Attempts  int       `json:"probe_attempts"`
	Failures  int       `json:"probe_failures"`
	Error     string    `json:"error,omitempty"`
}
