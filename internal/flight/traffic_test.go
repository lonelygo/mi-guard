package flight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
)

func TestControllerToFlightProjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connections" || r.Header.Get("Authorization") != "Bearer local" {
			t.Errorf("unexpected controller request")
		}
		_, _ = w.Write([]byte(`{"connections":[{"id":"private-id","metadata":{"host":"private.example","processPath":"/private/app","destinationGeoIP":["JP"]},"chains":["node-1","Group"],"upload":7,"download":10},{"id":"other","metadata":{"destinationGeoIP":["JP"]},"chains":["node-1","Group"],"upload":2,"download":3},{"id":"third","chains":["https://user:password@example.org"],"metadata":{"destinationGeoIP":[]}},{"id":"fourth","metadata":{"destinationGeoIP":["JP","US"]},"chains":["DIRECT"]}]}`))
	}))
	defer srv.Close()
	m := New(config.Config{FlightMap: config.FlightMap{RouteRegions: map[string]string{"Group": "HK"}}}, mihomo.New(srv.URL, "local"))
	defer m.Close()
	f := m.frame(context.Background(), time.Now())
	if !f.Online || len(f.Live) != 3 {
		t.Fatalf("bad live success/count: %v/%d", f.Online, len(f.Live))
	}
	var known bool
	for _, r := range f.Live {
		if r.Chain == "Group → node-1" {
			known = r.Active == 2 && r.UploadBytes == 9 && r.DownloadBytes == 13 && r.Exit != nil && r.Exit.ID == "HK" && r.Entry == nil && r.Target != nil && r.Target.ID == "JP"
		}
		if r.Chain == "DIRECT" && r.Target != nil {
			t.Fatal("ambiguous country rendered")
		}
	}
	if !known {
		t.Fatal("missing exact route grouping/observed country")
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-id", "private.example", "/private/app", "password", "example.org"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("private field in output")
		}
	}
}

func TestDeltasHaveAnObservedWindow(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Hour)
	r := route{label: "node", target: "US"}
	then := now.Add(-pollInterval)
	old := snapshot{"same": {r, count{10, 100, 1}, start}, "reset": {r, count{20, 30, 1}, start}, "gone": {r, count{9, 9, 1}, start}}
	next := snapshot{"same": {r, count{13, 109, 1}, start}, "reset": {r, count{1, 2, 1}, start}, "new": {r, count{2, 4, 1}, then.Add(time.Second)}, "missed": {r, count{999, 999, 1}, start}}
	got := deltas(old, next, then, now)
	if len(got) != 1 || got[0].DownloadBytes != 13 || got[0].UploadBytes != 5 || got[0].Connections != 1 {
		t.Fatalf("bad delta %v", got)
	}
	if len(deltas(nil, next, then, now)) != 0 || len(deltas(old, next, then.Add(-time.Hour), now)) != 0 {
		t.Fatal("replayed an observation gap")
	}
}

type unavailable struct{}

func (unavailable) ActiveTraffic(context.Context) ([]mihomo.TrafficConnection, error) {
	return nil, errors.New("controller diagnostic with private URL")
}
func TestOfflineFrameReadsHistoryWithoutReplayingOldTraffic(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := history.New(filepath.Join(t.TempDir(), "ledger.json"), 50, 1)
	m := New(config.Config{}, unavailable{})
	defer m.Close()
	var out bytes.Buffer
	if err := m.Display(ctx, store, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"online":false`) || strings.Contains(out.String(), "private URL") {
		t.Fatal("unexpected offline diagnostic")
	}
	// Use a time well within one minute to check calendar grouping at a precisely known instant.
	begin := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	at := begin.Add(time.Hour)
	b := history.RouteBucket{Start: begin, Exit: "US", RegionSource: history.RouteRegionPeer, Target: "SG", Chain: "node-1", DownloadBytes: 42, Connections: 1}
	if err := store.AddRouteBuckets(ctx, []history.RouteBucket{b}, "test-batch"); errors.Is(err, history.ErrAnalyticsUnavailable) {
		t.Skip("sqlite requires cgo")
	} else if err != nil {
		t.Fatal(err)
	}
	rows, err := routeHistory(ctx, store, at)
	if err != nil || len(rows) != 1 || rows[0].DownloadBytes != 42 || rows[0].Entry == nil || rows[0].Exit != nil {
		t.Fatalf("offline history mismatch %+v %v", rows, err)
	}
}
func TestCalendarWindowCrossesMonthAndDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, loc)
	p := calendarBoundaries(at)
	if len(p) != 3 || p[0].Day() != 26 || p[0].Month() != 10 || p[1].Day() != 1 || p[1].Hour() != 0 || !p[2].Equal(at) {
		t.Fatalf("bounds %#v", p)
	}
}
