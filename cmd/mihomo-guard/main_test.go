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

func TestInterruptReportsFailedRestoration(t *testing.T) {
	cancelled := fmt.Errorf("measurement: %w", context.Canceled)
	if !cleanInterrupt(errors.Join(cancelled, context.Canceled)) {
		t.Fatal("plain cancellation should exit cleanly")
	}
	if cleanInterrupt(errors.Join(cancelled, errors.New("could not restore selector"))) {
		t.Fatal("restoration failure hidden by Ctrl+C")
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
