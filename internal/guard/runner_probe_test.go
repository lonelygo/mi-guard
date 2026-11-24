package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestScanPersistsConciseProbeFailuresAndContinues(t *testing.T) {
	fixture, server := newControllerFixture(t)
	fixture.failing["a"], fixture.failing["b"] = true, true
	runner := testRunner(t, fixture, server)
	runner.Cfg.Speed.MonthlyBytes = 2048
	var progress []ScanProgress
	samples, err := runner.ScanWithProgress(context.Background(), []string{"a", "a", "b"}, "bench", 512, func(e ScanProgress) {
		progress = append(progress, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runner.Store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || len(snapshot.Samples) != 2 || snapshot.UsedBytes != 0 || snapshot.PendingBytes != 0 {
		t.Fatalf("scan produced %d samples, snapshot = %+v", len(samples), snapshot)
	}
	if len(progress) != 4 {
		t.Fatalf("expected start and settled result per unique node: %+v", progress)
	}
	for i, got := range samples {
		before, after := progress[2*i], progress[2*i+1]
		if before.Total != 2 || before.Completed != i || before.Node != got.Node || before.Result != nil ||
			after.Total != 2 || after.Completed != i+1 || after.Result == nil || *after.Result != got {
			t.Fatalf("unexpected progress around %s: %+v then %+v", got.Node, before, after)
		}
		if got.Error != "204 probe: HTTP 504" || got.Failures != 1 || got.Attempts != 1 || got.Bytes != 0 || got.Mbps != 0 {
			t.Fatalf("unexpected failed probe: %+v", got)
		}
		if snapshot.Samples[i].Error != got.Error {
			t.Fatalf("persisted error = %q, want %q", snapshot.Samples[i].Error, got.Error)
		}
	}
}

func TestProviderDiscoveryOutageRemainsUnattributed(t *testing.T) {
	providerReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxies/provider-node", "/proxies/provider-node/delay":
			http.NotFound(w, r)
		case "/providers/proxies":
			providerReads++
			if providerReads == 1 {
				_, _ = w.Write([]byte(`{"providers":{"airport":{"vehicleType":"HTTP","proxies":[{"name":"provider-node","type":"Trojan"}]}}}`))
				return
			}
			http.Error(w, `{"message":"provider storage unavailable"}`, http.StatusGatewayTimeout)
		case "/proxies/bench":
			_ = json.NewEncoder(w).Encode(model.Proxy{Name: "bench", Type: "Selector", Now: "provider-node", All: []string{"provider-node"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner := testRunner(t, nil, server)
	runner.Cfg.Speed.MonthlyBytes = 2048
	sample, err := runner.Measure(context.Background(), "provider-node", "bench", 512)
	if err == nil || !strings.Contains(err.Error(), "/providers/proxies") || !strings.Contains(err.Error(), "504") {
		t.Fatalf("Measure() error = %v, want provider discovery 504", err)
	}
	if sample != (model.Sample{}) {
		t.Fatalf("unattributed sample = %+v", sample)
	}
	snapshot, snapshotErr := runner.Store.Snapshot(context.Background())
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	if snapshot.UsedBytes != 0 || snapshot.PendingBytes != 0 || len(snapshot.Samples) != 0 {
		t.Fatalf("unattributed snapshot = %+v", snapshot)
	}
}
