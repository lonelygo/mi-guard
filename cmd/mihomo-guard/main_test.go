package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestHelpAndVersionWithoutConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"-v"}, []string{"mihomo-guard " + buildVersion() + "\n"}},
		{[]string{"--version"}, []string{"mihomo-guard " + buildVersion() + "\n"}},
		{[]string{"-h"}, []string{"-h, --help", "-v, --version"}},
		{[]string{"--help"}, []string{"-h, --help", "-v, --version"}},
		{[]string{"bench", "-h"}, []string{"mihomo-guard bench", "-apply"}},
		{[]string{"bench", "--help"}, []string{"mihomo-guard bench", "-apply"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			original := os.Stdout
			os.Stdout = out
			defer func() { os.Stdout = original }()
			if err := run(context.Background(), tc.args); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(out.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(got), want) {
					t.Errorf("stdout %q is missing %q", got, want)
				}
			}
		})
	}
}

func TestInterruptReportsFailedRestoration(t *testing.T) {
	cancelled := fmt.Errorf("measurement: %w", context.Canceled)
	if !cleanInterrupt(errors.Join(cancelled, context.Canceled)) {
		t.Fatal("plain cancellation should exit cleanly")
	}
	if cleanInterrupt(errors.Join(cancelled, errors.New("could not restore selector"))) {
		t.Fatal("restoration failure hidden by Ctrl+C")
	}
}

func TestImplicitManagedGroupIsNeverFuzzyApplied(t *testing.T) {
	fixture, server := newCommandController(t)
	r := commandRunner(t, server)
	r.Cfg.Groups[0].Name = "war" // only "warp" exists in the controller
	err := bench(context.Background(), r, &config.CoreFile{}, benchOptions{apply: true})
	if err == nil || !strings.Contains(err.Error(), "exact name") || len(fixture.writes) != 0 || len(fixture.delays) != 0 {
		t.Fatalf("implicit fuzzy group: err=%v writes=%v probes=%v", err, fixture.writes, fixture.delays)
	}
}

type commandController struct {
	mu              sync.Mutex
	proxies         map[string]model.Proxy
	reads           []string
	delays          []string
	writes          []string
	connectionReads int
}

func newCommandController(t *testing.T) (*commandController, *httptest.Server) {
	t.Helper()
	fixture := &commandController{proxies: map[string]model.Proxy{
		"warp":  {Name: "warp", Type: "Selector", Now: "a", All: []string{"a", "b"}},
		"bench": {Name: "bench", Type: "Selector", Now: "a", All: []string{"a", "b"}},
		"a":     {Name: "a", Type: "Trojan", Provider: "airport"},
		"b":     {Name: "b", Type: "Vless", Provider: "airport"},
	}}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	return fixture, server
}

func (f *commandController) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/version" {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
		return
	}
	if r.URL.Path == "/connections" {
		f.connectionReads++
		_ = json.NewEncoder(w).Encode(map[string]any{"connections": []any{}})
		return
	}
	if r.URL.Path == "/proxies" {
		_ = json.NewEncoder(w).Encode(map[string]any{"proxies": f.proxies})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/proxies/") {
		http.NotFound(w, r)
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/proxies/")
	delay := strings.HasSuffix(raw, "/delay")
	if delay {
		raw = strings.TrimSuffix(raw, "/delay")
	}
	name, _ := url.PathUnescape(raw)
	if delay {
		f.delays = append(f.delays, name)
		_ = json.NewEncoder(w).Encode(map[string]int{"delay": 20})
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.reads = append(f.reads, name)
		proxy, ok := f.proxies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(proxy)
	case http.MethodPut:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		proxy := f.proxies[name]
		proxy.Now = body.Name
		f.proxies[name] = proxy
		f.writes = append(f.writes, name+"="+body.Name)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func commandRunner(t *testing.T, server *httptest.Server) *guard.Runner {
	t.Helper()
	return &guard.Runner{
		Cfg: config.Config{
			Mihomo:    config.Core{TestGroup: "bench"},
			Speed:     config.Speed{LivenessURL: "https://example.test/204", Timeout: time.Second},
			Schedule:  config.Schedule{HeartbeatTimeout: 100 * time.Millisecond},
			Selection: config.Selection{NearBestPct: 20},
			Groups:    []config.Group{{Name: "warp", Mode: config.GroupModeSelector}},
		},
		Core:  mihomo.New(server.URL, ""),
		Store: history.New(filepath.Join(t.TempDir(), "history.json"), 10, 1),
	}
}

func TestCheckManagedGroupAcceptsStandaloneSelectorWithoutChainRead(t *testing.T) {
	fixture, server := newCommandController(t)
	runner := commandRunner(t, server)
	group := runner.Cfg.Groups[0]

	nodes, err := checkManagedGroup(context.Background(), runner, group)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(nodes, ","); got != "a,b" {
		t.Fatalf("candidates = %q", got)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.delays) != 0 {
		t.Fatalf("wiring check performed an unsolicited heartbeat: %v", fixture.delays)
	}
	for _, name := range fixture.reads {
		if name == "" || name == "chain" {
			t.Fatalf("standalone check read %q: %v", name, fixture.reads)
		}
	}
}

func TestCheckGeoListenerErrorShowsEndpointAndJoinedEmoji(t *testing.T) {
	const chain = "👩‍💻 示例链"
	f, server := newCommandController(t)
	f.proxies[chain] = model.Proxy{Name: chain, Type: "WireGuard", Dialer: "warp"}
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer reject.Close()
	runner := commandRunner(t, server)
	runner.Cfg.Speed.GeoURL = "https://example.test/geo"
	_, err := checkManagedGroup(context.Background(), runner, config.Group{Name: "warp", Chain: chain, GeoProxy: reject.URL})
	if err == nil || !strings.Contains(err.Error(), reject.URL) || !strings.Contains(err.Error(), chain) {
		t.Fatalf("geo listener diagnostic = %v", err)
	}
}

func TestStatusProbeUsesStandaloneSelectorLivePath(t *testing.T) {
	fixture, server := newCommandController(t)
	runner := commandRunner(t, server)
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	stdout := os.Stdout
	os.Stdout = sink
	err = status(context.Background(), runner, true)
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if got := strings.Join(fixture.delays, ","); got != "warp" {
		t.Fatalf("standalone status probes %q", got)
	}
}
