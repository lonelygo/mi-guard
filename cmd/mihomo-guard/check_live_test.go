package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/mihomo"
)

func TestCheckProbesOncePerMemberAndSeparatesFailures(t *testing.T) {
	var mu sync.Mutex
	hits := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" || req.URL.Query().Get("url") != "https://probe.test/204" {
			t.Errorf("unexpected probe: %s %s", req.Method, req.URL)
		}
		mu.Lock()
		hits[req.URL.Path]++
		mu.Unlock()
		switch req.URL.Path {
		case "/proxies/ok/delay":
			fmt.Fprint(w, `{"delay":0}`)
		case "/proxies/offline/delay":
			w.WriteHeader(http.StatusGatewayTimeout)
		case "/proxies/unknown/delay":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected controller path %s", req.URL.Path)
		}
	}))
	defer server.Close()
	r := &guard.Runner{Core: mihomo.New(server.URL, ""), Cfg: config.Config{
		Speed: config.Speed{LivenessURL: "https://probe.test/204"}, Schedule: config.Schedule{HeartbeatTimeout: time.Second},
	}}
	probes := make(map[string]memberProbe)
	for _, group := range [][]string{{"ok", "offline"}, {"ok", "unknown"}} {
		if err := probeMembers(context.Background(), r, group, probes); err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) != 3 || hits["/proxies/ok/delay"] != 1 || probes["ok"].label() != "0 ms" ||
		probes["offline"].label() != "失败 (HTTP 504)" || probes["unknown"].label() != "未知（控制器未能确认）" {
		t.Fatalf("requests=%v, probes=%v", hits, probes)
	}
}
