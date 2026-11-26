package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"github.com/lonelygo/mi-guard/internal/model"
)

func TestCheckBandwidthUsesMatchingProviderScopeAndTime(t *testing.T) {
	now := time.Now()
	profile := model.DownloadProfile{Streams: 1, TotalBytes: 4 << 20, Target: strings.Repeat("a", 64)}
	proxies := map[string]model.Proxy{
		"a": {Name: "a", Type: "Vless", Provider: "pool"},
		"b": {Name: "b", Type: "Trojan", Provider: "pool", Dialer: "front"},
	}
	good := model.Sample{Node: "a", Provider: "pool", Scope: "front", Kind: "bench", At: now.Add(-time.Hour), Mbps: 32, Profile: profile}
	chain := good
	chain.Node, chain.Scope = "b", "chain:b"
	samples := []model.Sample{good, chain}
	for _, change := range []func(*model.Sample){
		func(s *model.Sample) { s.Provider = "previous" },
		func(s *model.Sample) { s.Scope = "chain:a" },
		func(s *model.Sample) { s.Kind = "heartbeat" },
		func(s *model.Sample) { s.At = now.Add(time.Minute) },
		func(s *model.Sample) { s.Error = "download failed" },
		func(s *model.Sample) { s.Node = "b" },
		func(s *model.Sample) { s.Profile = model.DownloadProfile{} },
	} {
		invalid := good
		invalid.At, invalid.Mbps = now, 999
		change(&invalid)
		samples = append(samples, invalid)
	}
	got := latestBandwidth(samples, proxies, now)
	if len(got) != 2 || got["a"] != good || got["b"] != chain {
		t.Fatalf("misattributed measurements: %+v", got)
	}
}

func TestCheckReportDistinguishesFreshProbesHistoricalBandwidthAndTenure(t *testing.T) {
	now := time.Now()
	measured := model.Sample{At: now.Add(-2 * time.Hour), Mbps: 32, Kind: "bench",
		Profile: model.DownloadProfile{Streams: 1, TotalBytes: 4 << 20, Target: strings.Repeat("a", 64)}}
	tenure := history.SelectionTenure{Node: "a", Since: now.Add(-time.Hour), LastSeen: now, GuardSelected: true, Longest: map[string]time.Duration{"a": time.Hour, "b": 2 * time.Hour}}
	group := config.Group{Name: "前置组", Chain: "链"}
	probes := map[string]memberProbe{"a": {delay: 17}}
	var report strings.Builder
	err := reportLiveGroup(&report, group, "a", []string{"a", "b"}, probes, measured, tenure, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"链", "前置组", "本次探测可用 1/2，1 个未能确认", "当前 a", "HTTP 延迟 17 ms", "32.00 Mbps / 3.81 MiB/s", "bench，1 流", "已过期", "已选用 1h0m0s（自 guard 切换）", "最长选用记录 b 2h0m0s"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("missing %q in %s", want, report.String())
		}
	}
}

func TestCheckObservesStableSelectionAcrossChecksWithoutDownloads(t *testing.T) {
	f := checkFixture()
	r, file := checkTestRunner(t, f.serveHTTP)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	if err := r.Store.Record(ctx, model.Sample{Node: "a", Provider: "pool", Scope: "front", Kind: "bench", At: at, Mbps: 70, LatencyMS: 5, Profile: r.Cfg.DownloadProfile(4, 16<<20)}); err != nil {
		t.Fatal(err)
	}
	inspect := func() string {
		t.Helper()
		var text strings.Builder
		if err := checkGroups(ctx, r, &text); err != nil {
			t.Fatal(err)
		}
		return text.String()
	}
	if text := inspect(); !strings.Contains(text, "本次探测可用 2/2") || !strings.Contains(text, "bench，4 流") ||
		!strings.Contains(text, "首次观察，尚无更早记录") {
		t.Fatalf("first report:\n%s", text)
	}
	first, err := r.Store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.Store = history.New(file, 10, 1)
	inspect()
	f.mu.Lock()
	p := f.proxies["managed"]
	p.Now = "b" // simulate an operator at the fixture
	f.proxies["managed"] = p
	f.mu.Unlock()
	if text := inspect(); !strings.Contains(text, "当前 b") ||
		!strings.Contains(text, "最近带宽 暂无测速记录") || !strings.Contains(text, "首次观察，尚无更早记录") {
		t.Fatalf("manual selection report:\n%s", text)
	}
	after, err := r.Store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, current := first.Selections["managed"], after.Selections["managed"]
	if old.Node != "a" || current.Node != "b" || !current.Since.After(old.LastSeen) || current.GuardSelected || current.Longest["a"] <= 0 ||
		after.UsedBytes != 0 || after.PendingBytes != 0 || len(after.Samples) != 1 || len(after.Heartbeats) != 0 {
		t.Fatalf("after repeated check: %+v", after)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delays) != 6 || len(f.writes) != 0 || f.connectionReads != 0 {
		t.Fatalf("unexpected probe count, selection writes, or download route checks: %+v", f)
	}
}

func TestCheckAndDaemonKeepConfirmedSelectionThroughRestart(t *testing.T) {
	f := checkFixture()
	r, file := checkTestRunner(t, f.serveHTTP)
	ctx := context.Background()
	group := r.Cfg.Groups[0]
	if _, _, err := r.ObservePrimary(ctx, group.Name); err != nil {
		t.Fatal(err)
	}
	ranked := []model.Sample{{Node: "b", Kind: "bench", Mbps: 20}}
	if _, err := r.Apply(ctx, group, "a", ranked); err != nil {
		t.Fatal(err)
	}
	confirmed, err := r.Store.Snapshot(ctx)
	if err != nil || !confirmed.Selections[group.Name].GuardSelected {
		t.Fatalf("confirmed selection = %+v, %v", confirmed.Selections, err)
	}
	if _, err := r.Apply(ctx, group, "b", ranked); err != nil {
		t.Fatal(err)
	}
	r.Store = history.New(file, 10, 1)
	r.Cfg.Schedule.FullInterval = 24 * time.Hour
	r.Cfg.Schedule.HeartbeatInterval = time.Hour
	if err := r.Store.MarkSweep(ctx, time.Now()); err != nil { // only the heartbeat is due
		t.Fatal(err)
	}
	heartbeatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- r.Daemon(heartbeatCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	var after history.Snapshot
	for len(after.Heartbeats) == 0 {
		after, err = r.Store.Snapshot(heartbeatCtx)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Heartbeats) == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	got, switched := after.Selections[group.Name], confirmed.Selections[group.Name]
	if got.Node != "b" || !got.GuardSelected || !got.Since.Equal(switched.Since) || !got.LastSeen.After(switched.LastSeen) ||
		got.Longest["b"] != got.LastSeen.Sub(got.Since) || after.Heartbeats[0].Attempts != 1 || after.UsedBytes != 0 || len(after.Samples) != 0 {
		t.Fatalf("keep and daemon heartbeat: %+v", after)
	}
	var report strings.Builder
	if err := checkGroups(ctx, r, &report); err != nil || !strings.Contains(report.String(), "自 guard 切换") {
		t.Fatalf("check after daemon restart:\n%s\nerror: %v", report.String(), err)
	}
}

func TestCheckPreservesLiveValidationErrors(t *testing.T) {
	f := checkFixture()
	r, _ := checkTestRunner(t, f.serveHTTP)
	ctx := context.Background()
	bench := f.proxies["bench"]
	bench.Type = "URLTest"
	f.proxies["bench"] = bench
	if err := check(ctx, r); err == nil || err.Error() != "live core is missing the configured test selector or its candidates" {
		t.Fatalf("invalid live bench selector: %v", err)
	}
	bench.Type = "Selector"
	f.proxies["bench"] = bench
	r.Cfg.Mihomo.TestProxy = "invalid://listener"
	if err := check(ctx, r); err == nil || !strings.Contains(err.Error(), `cannot verify live test listener invalid://listener for current member "a"`) {
		t.Fatalf("bad live bench listener: %v", err)
	}
	r.Cfg.Groups[0].Mode, r.Cfg.Groups[0].Chain = config.GroupModeChain, "chain"
	f.proxies["chain"] = model.Proxy{Name: "chain", Type: "WireGuard", Dialer: "wrong"}
	var text strings.Builder
	if err := checkGroups(ctx, r, &text); err == nil || err.Error() != "invalid live target managed" || text.Len() != 0 {
		t.Fatalf("bad live dialer: %v, output %q", err, text.String())
	}
	f.proxies["chain"] = model.Proxy{Name: "chain", Type: "WireGuard", Dialer: "managed"}
	r.Cfg.Groups[0].GeoProxy, r.Cfg.Speed.GeoURL = "invalid://geo-listener", "https://geo.invalid/"
	if err := checkGroups(ctx, r, &text); err == nil || !strings.Contains(err.Error(), `cannot verify live geo listener invalid://geo-listener for "chain"`) {
		t.Fatalf("bad live geo listener: %v", err)
	}
}

func checkFixture() *commandController {
	return &commandController{proxies: map[string]model.Proxy{
		"managed": {Name: "managed", Type: "Selector", Now: "a", All: []string{"a", "b"}},
		"bench":   {Name: "bench", Type: "Selector", Now: "a", All: []string{"a", "b"}},
		"a":       {Name: "a", Type: "Trojan", Provider: "pool"},
		"b":       {Name: "b", Type: "Vless", Provider: "pool"},
	}}
}

func checkTestRunner(t *testing.T, controller http.HandlerFunc) (*guard.Runner, string) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	file := filepath.Join(t.TempDir(), "history.json")
	server := httptest.NewServer(controller)
	t.Cleanup(server.Close)
	return &guard.Runner{
		Cfg: config.Config{
			Mihomo:    config.Core{TestGroup: "bench"},
			Speed:     config.Speed{URL: "https://download.invalid/large", LivenessURL: "https://health.invalid/204", Timeout: time.Second},
			Schedule:  config.Schedule{HeartbeatTimeout: time.Second},
			Groups:    []config.Group{{Name: "managed", Mode: config.GroupModeSelector}},
			Selection: config.Selection{MaxAge: 2 * time.Hour},
		},
		Core: mihomo.New(server.URL, ""), Store: history.New(file, 10, 1),
	}, file
}

func TestCheckRequiresVerifiedTunnelWithoutDownloading(t *testing.T) {
	f := checkFixture()
	var mu sync.Mutex
	var clientPort string
	var route []string
	r, _ := checkTestRunner(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/connections" {
			f.serveHTTP(w, req)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"connections": []any{
			map[string]any{"chains": route, "metadata": map[string]string{"sourcePort": clientPort}},
		}})
	})
	// Accept CONNECT and inspect every byte after the headers. The fixture
	// never forwards a tunnel or contacts the target.
	payload := make(chan string, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect || req.Host != "download.invalid:443" {
			t.Errorf("unexpected listener request %s %s", req.Method, req.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		mu.Lock()
		_, clientPort, err = net.SplitHostPort(req.RemoteAddr)
		mu.Unlock()
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprint(buf, "HTTP/1.1 200 OK\r\n\r\n")
		buf.Flush()
		if b, err := buf.ReadByte(); err == nil {
			payload <- fmt.Sprintf("unexpected payload byte %x", b)
			return
		}
		payload <- ""
	}))
	defer proxy.Close()
	r.Cfg.Mihomo.TestProxy = proxy.URL
	out, err := os.CreateTemp(t.TempDir(), "check-output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stdout := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = stdout }()
	for _, valid := range []bool{true, false} {
		mu.Lock()
		route = []string{"bench", "a"}
		if !valid {
			route = []string{"elsewhere", "b"}
		}
		mu.Unlock()
		err := check(context.Background(), r)
		if valid && err != nil || !valid && (err == nil || !strings.Contains(err.Error(), "cannot verify live test listener")) {
			t.Fatalf("valid route %t: %v", valid, err)
		}
		select {
		case got := <-payload:
			if got != "" {
				t.Fatal(got)
			}
		case <-time.After(time.Second):
			t.Fatal("check left a tunnel open")
		}
	}
	output, err := os.ReadFile(out.Name())
	if err != nil || strings.Count(string(output), "ready: Mihomo test, bench (2 members)\n") != 1 {
		t.Fatalf("ready line after good and bad route proofs: %q, %v", output, err)
	}
}
