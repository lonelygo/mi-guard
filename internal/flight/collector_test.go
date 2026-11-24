//go:build cgo

package flight

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
)

type controlledSource struct {
	polls   chan struct{}
	replies chan []mihomo.TrafficConnection
}

func (s controlledSource) ActiveTraffic(ctx context.Context) ([]mihomo.TrafficConnection, error) {
	select {
	case s.polls <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case reply := <-s.replies:
		if reply == nil {
			return nil, errors.New("offline")
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCollectorToOfflineMap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	src := controlledSource{make(chan struct{}), make(chan []mihomo.TrafficConnection)}
	m := New(config.Config{}, src)
	store := history.New(filepath.Join(t.TempDir(), "old.json"), 50, 1)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); m.collect(ctx, store, slog.Default(), ticks) }()
	t.Cleanup(func() { cancel(); <-done; m.Close() })
	start := time.Now().Add(-time.Hour)
	step := func(data []mihomo.TrafficConnection) {
		t.Helper()
		select {
		case <-src.polls:
		case <-ctx.Done():
			t.Fatal("collector did not request next snapshot")
		}
		src.replies <- data
		select {
		case ticks <- time.Now():
		case <-ctx.Done():
			t.Fatal("collector did not settle poll")
		}
	}
	active := func(n int64) []mihomo.TrafficConnection {
		return []mihomo.TrafficConnection{{ID: "A", Start: start, Chains: []string{"node"}, Download: n}}
	}
	step(active(1000))
	step(active(1042))
	step(nil)
	step(active(2000))
	step([]mihomo.TrafficConnection{})
	cancel()
	<-done
	rows, err := routeHistory(context.Background(), store, time.Now().Add(time.Second))
	if err != nil || len(rows) != 1 || rows[0].DownloadBytes != 42 || rows[0].Connections != 0 {
		t.Fatalf("persisted chart after a gap and close: %+v, %v", rows, err)
	}
}
