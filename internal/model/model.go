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

const MaxDownloadStreams = 8

// DownloadProfile describes the requested test, even when a time limit or
// failed probe ends it early. Its zero value identifies unlabelled history.
type DownloadProfile struct {
	Streams    int
	TotalBytes int64
	Target     string // fingerprint of download and liveness endpoints and timeouts
}

func (p DownloadProfile) Known() bool {
	return p.Streams >= 1 && p.Streams <= MaxDownloadStreams && p.TotalBytes >= int64(p.Streams) && p.Target != ""
}

type Sample struct {
	At        time.Time       `json:"time"`
	Node      string          `json:"node"`
	ProxyType string          `json:"-"` // controller metadata for the live bench result
	Profile   DownloadProfile `json:"-"` // the v1 ledger encodes this in its existing scope field
	Provider  string          `json:"provider,omitempty"`
	Scope     string          `json:"scope"` // front, or a chain name for a heartbeat
	Kind      string          `json:"kind"`  // sweep, quick, confirm, bench, heartbeat
	Mbps      float64         `json:"download_mbps"`
	Bytes     int64           `json:"bytes"`
	ElapsedMS int64           `json:"elapsed_ms"`
	LatencyMS int             `json:"probe_latency_ms"`
	Attempts  int             `json:"probe_attempts"`
	Failures  int             `json:"probe_failures"`
	Error     string          `json:"error,omitempty"`
}

// ProbeLatency returns a successful HTTP 204 latency from a bandwidth
// measurement. A zero-millisecond response is distinguishable from the zero
// value only after the download attempt increments Attempts.
func (s Sample) ProbeLatency() (int, bool) {
	switch s.Kind {
	case "bench", "sweep", "quick", "confirm":
	default:
		return 0, false
	}
	if s.LatencyMS < 0 || (s.Attempts < 2 && (s.LatencyMS == 0 || s.Failures > 0)) {
		return 0, false
	}
	return s.LatencyMS, true
}
