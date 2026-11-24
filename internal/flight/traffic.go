// Package flight projects Mihomo's active connection snapshots into regional
// routes. Only region codes, safe route labels and aggregated counters persist.
package flight

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
)

type source interface {
	ActiveTraffic(context.Context) ([]mihomo.TrafficConnection, error)
}
type route struct{ label, entry, via, provenance, target string }
type count struct{ up, down, connections int64 }
type reading struct {
	r route
	count
	start time.Time
}
type snapshot map[string]reading

type Monitor struct {
	core       source
	country    *CountryDB
	names      map[string]string
	explicitDB bool
	geoCache   map[netip.Addr]string
}

func New(c config.Config, core source) *Monitor {
	return &Monitor{
		core: core, country: discoverCountry(c.FlightMap.CountryDB, c.Mihomo.ConfigPath),
		names: c.FlightMap.RouteRegions, explicitDB: c.FlightMap.CountryDB != "",
		geoCache: make(map[netip.Addr]string),
	}
}
func (m *Monitor) Close() { m.country.close() }

func (m *Monitor) locate(raw string) string {
	if m.country == nil || raw == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	ip, err := netip.ParseAddr(strings.Trim(raw, "[]"))
	if err != nil {
		return ""
	}
	if v, ok := m.geoCache[ip]; ok {
		return v
	}
	v := m.country.country(ip)
	if len(m.geoCache) >= 8192 {
		clear(m.geoCache)
	}
	m.geoCache[ip] = v
	return v
}

func routeLabel(chains []string) string {
	if len(chains) == 0 {
		return cleanLabel("")
	}
	labels := make([]string, 0, min(len(chains), 12))
	if len(chains) > 12 {
		labels = append(labels, "…")
	}
	for i := min(len(chains), 12) - 1; i >= 0; i-- {
		labels = append(labels, cleanLabel(chains[i]))
	}
	return strings.Join(labels, " → ")
}

func cleanLabel(name string) string {
	if name == "" {
		return "未注明线路"
	}
	ipText := name
	if host, _, err := net.SplitHostPort(name); err == nil {
		ipText = host
	}
	if _, err := netip.ParseAddr(strings.Trim(ipText, "[]")); err == nil || strings.ContainsAny(name, "/@\\?#") || len(name) > 256 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		hash := sha256.Sum256([]byte(name))
		return fmt.Sprintf("私有线路 · %x", hash[:5])
	}
	return name
}
func (m *Monitor) read(ctx context.Context) (snapshot, error) {
	connections, err := m.core.ActiveTraffic(ctx)
	if err != nil {
		return nil, err
	}
	out := make(snapshot, len(connections))
	for _, c := range connections {
		if c.ID == "" || c.Upload < 0 || c.Download < 0 {
			continue
		}
		var leaf string
		if len(c.Chains) > 0 {
			leaf = c.Chains[0]
		}
		r := route{label: routeLabel(c.Chains)}
		// The first chain entry is the outbound leaf. Prefer an explicit mapping
		// for it, followed by an explicit mapping of one of its enclosing groups.
		for _, name := range c.Chains {
			if code := m.names[name]; code != "" {
				r.via = code
				r.provenance = history.RouteRegionConfigured
				break
			}
		}
		for _, v := range c.Metadata.TargetCountries {
			if code := region(v); code != "" {
				if r.target != "" && code != r.target {
					r.target = ""
					break
				}
				r.target = code
			}
		}
		if r.target == "" {
			r.target = m.locate(c.Metadata.Destination)
		}
		switch strings.ToUpper(leaf) {
		case "DIRECT", "COMPATIBLE":
			if r.target == "" {
				r.target = m.locate(c.Metadata.Peer)
			}
		case "", "REJECT", "REJECT-DROP", "PASS":
		default:
			r.entry = m.locate(c.Metadata.Peer)
		}
		out[c.ID] = reading{r: r, count: count{up: c.Upload, down: c.Download, connections: 1}, start: c.Start}
	}
	return out, nil
}

func sum(totals map[route]count, r route, n count) {
	v := totals[r]
	v.up = addCounter(v.up, n.up)
	v.down = addCounter(v.down, n.down)
	v.connections = addCounter(v.connections, n.connections)
	totals[r] = v
}

func addCounter(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// Traffic before the first observation, across an outage or before a counter
// reset cannot be attributed to this time window. Never replay it into history.
func deltas(old, current snapshot, last, now time.Time) []history.RouteBucket {
	if old == nil || !now.After(last) || now.Sub(last) > 3*pollInterval {
		return nil
	}
	grouped := map[route]count{}
	for id, next := range current {
		before, existed := old[id]
		var d count
		if existed && before.start.Equal(next.start) {
			if next.up < before.up || next.down < before.down {
				continue
			}
			d.up = next.up - before.up
			d.down = next.down - before.down
		} else if !next.start.Before(last) && !next.start.After(now) {
			d = next.count
		} else {
			continue
		}
		if d.up != 0 || d.down != 0 || d.connections != 0 {
			sum(grouped, next.r, d)
		}
	}
	out := make([]history.RouteBucket, 0, len(grouped))
	for r, n := range grouped {
		out = append(out, history.RouteBucket{
			Start: now, Entry: r.entry, Exit: r.via, RegionSource: r.provenance, Target: r.target,
			Chain: r.label, DownloadBytes: n.down, UploadBytes: n.up, Connections: n.connections,
		})
	}
	return out
}

const pollInterval = 10 * time.Second

func (m *Monitor) Record(ctx context.Context, store *history.Store, logger *slog.Logger) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	m.collect(ctx, store, logger, ticker.C)
}

func (m *Monitor) collect(ctx context.Context, store *history.Store, logger *slog.Logger, ticks <-chan time.Time) {
	var previous snapshot
	var last time.Time
	var pending []history.RouteBucket
	var token string
	var lastWarning time.Time
	for {
		now := time.Now()
		if len(pending) == 0 {
			cur, err := m.read(ctx)
			now = time.Now()
			if err != nil {
				previous = nil
			} else {
				pending = deltas(previous, cur, last, now)
				previous, last = cur, now
				if len(pending) > 0 {
					token = rand.Text()
				}
			}
		}
		if len(pending) > 0 {
			if err := store.AddRouteBuckets(ctx, pending, token); err == nil {
				pending = nil
			} else if ctx.Err() == nil && time.Since(lastWarning) > time.Minute {
				logger.Warn("flight history write failed", "error", err)
				lastWarning = now
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

type place struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CountryCode string `json:"countryCode"`
}

func placeFor(code string) *place {
	if region(code) == "" {
		return nil
	}
	return &place{code, code, code}
}

type pathRow struct {
	Chain         string `json:"chain"`
	Entry         *place `json:"entry,omitempty"`
	Exit          *place `json:"exit,omitempty"`
	Target        *place `json:"target,omitempty"`
	UploadBytes   int64  `json:"uploadBytes"`
	DownloadBytes int64  `json:"downloadBytes"`
}
type liveRow struct {
	pathRow
	ID     string `json:"id"`
	Active int64  `json:"activeConnections"`
}
type pastRow struct {
	pathRow
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Connections int64     `json:"connections"`
}
type Frame struct {
	GeneratedAt  time.Time `json:"generatedAt"`
	Online       bool      `json:"online"`
	Live         []liveRow `json:"live"`
	Buckets      []pastRow `json:"buckets"`
	LiveError    string    `json:"liveError,omitempty"`
	HistoryError string    `json:"historyError,omitempty"`
	GeoNotice    string    `json:"geoNotice,omitempty"`
}

func row(r route, n count) pathRow {
	p := pathRow{Chain: r.label, Entry: placeFor(r.entry), Target: placeFor(r.target), UploadBytes: n.up, DownloadBytes: n.down}
	if r.provenance == history.RouteRegionConfigured {
		p.Exit = placeFor(r.via)
	} else if r.provenance == history.RouteRegionPeer && p.Entry == nil {
		p.Entry = placeFor(r.via)
	}
	return p
}
func (m *Monitor) frame(ctx context.Context, now time.Time) Frame {
	f := Frame{GeneratedAt: now, Live: []liveRow{}, Buckets: []pastRow{}}
	if m.country == nil {
		if m.explicitDB {
			f.GeoNotice = "指定的国家地区数据库无法读取。仍显示内核报告和手动配置的地区。"
		} else {
			f.GeoNotice = "当前使用内核报告和手动配置的地区。可指定本机 Country.mmdb 补全 IP 所在地区。"
		}
	}
	active, err := m.read(ctx)
	if err != nil {
		f.LiveError = "暂时无法读取 Mihomo 的活跃连接。历史航线仍可查看。"
		return f
	}
	f.Online = true
	grouped := map[route]count{}
	for _, v := range active {
		sum(grouped, v.r, v.count)
	}
	for r, n := range grouped {
		key := sha256.Sum256([]byte(strings.Join([]string{r.label, r.entry, r.via, r.provenance, r.target}, "\x00")))
		f.Live = append(f.Live, liveRow{row(r, n), hex.EncodeToString(key[:12]), n.connections})
	}
	slices.SortFunc(f.Live, func(a, b liveRow) int { return strings.Compare(a.ID, b.ID) })
	return f
}
