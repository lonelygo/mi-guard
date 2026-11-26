//go:build cgo

package history

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestAnalyticsReadOnlyMissingDoesNotCreateFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	store := New(filepath.Join(directory, "history.json"), 3, 1)
	decisions, err := store.RecentDecisions(context.Background(), 10)
	if err != nil || decisions != nil {
		t.Fatalf("missing decisions = %#v, %v", decisions, err)
	}
	if _, err := store.DecisionByID(context.Background(), "missing"); !errors.Is(err, ErrDecisionNotFound) {
		t.Fatalf("missing decision error = %v", err)
	}
	buckets, err := store.RouteBuckets(context.Background(), time.Time{}, time.Time{}, RouteMinute)
	if err != nil || buckets != nil {
		t.Fatalf("missing routes = %#v, %v", buckets, err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only analytics created a directory: %v", err)
	}
}

func TestDecisionJournalRoundTripIsIdempotentAndSeparateFromBudget(t *testing.T) {
	store := testStore(t, 3)
	if err := store.Record(context.Background(), model.Sample{Node: "a", Scope: "front", Kind: "bench", Mbps: 7}); err != nil {
		t.Fatal(err)
	}
	legacyBefore, err := os.ReadFile(store.file)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.November, 24, 13, 5, 0, 123, time.FixedZone("CST", 8*60*60))
	observedAt := at.UTC()
	decision := Decision{
		ID: "event-1", At: at, Source: "bench", Group: "US", Before: "a", After: "b", Action: "switched",
		Candidates: []DecisionCandidate{{
			Node: "b", Kind: "sweep", ObservedAt: &observedAt, Mbps: 12,
			ValidationOrder: 1, Validation: "passed", Selected: true,
		}},
	}
	if err := store.RecordDecision(context.Background(), decision); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDecision(context.Background(), decision); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	conflict := decision
	conflict.After = "c"
	if err := store.RecordDecision(context.Background(), conflict); err == nil {
		t.Fatal("accepted conflicting decision id")
	}
	got, err := store.DecisionByID(context.Background(), decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision.At = decision.At.UTC()
	if !reflect.DeepEqual(got, decision) {
		t.Fatalf("decision = %#v, want %#v", got, decision)
	}
	recent, err := store.RecentDecisions(context.Background(), 1)
	if err != nil || len(recent) != 1 || recent[0].ID != decision.ID {
		t.Fatalf("recent = %#v, %v", recent, err)
	}
	legacyAfter, err := os.ReadFile(store.file)
	if err != nil || string(legacyBefore) != string(legacyAfter) {
		t.Fatalf("analytics changed JSON budget ledger: %v", err)
	}
	info, err := os.Stat(store.analyticsPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("analytics database mode = %v, %v", info, err)
	}
}

func TestAnalyticsPreservesExistingFilePermissions(t *testing.T) {
	store := testStore(t, 3)
	first := Decision{ID: "first", At: store.now(), Source: "test", Group: "g", Action: "kept"}
	if err := store.RecordDecision(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.analyticsPath(), 0o640); err != nil {
		t.Fatal(err)
	}
	second := Decision{ID: "second", At: store.now().Add(time.Second), Source: "test", Group: "g", Action: "kept"}
	if err := store.RecordDecision(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.analyticsPath())
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("existing database mode = %v, %v", info, err)
	}
}

func TestDecisionJournalRetentionKeepsNewestEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("retention test performs durable sqlite transactions")
	}
	store := testStore(t, 3)
	base := store.now().UTC()
	for index := 0; index < decisionRetention+2; index++ {
		decision := Decision{ID: fmt.Sprintf("event-%04d", index), At: base.Add(time.Duration(index) * time.Second), Source: "test", Group: "g", Action: "kept"}
		if err := store.RecordDecision(context.Background(), decision); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.RecentDecisions(context.Background(), decisionRetention+10)
	if err != nil || len(got) != decisionRetention {
		t.Fatalf("retained decisions = %d, %v", len(got), err)
	}
	if got[0].ID != "event-1001" || got[len(got)-1].ID != "event-0002" {
		t.Fatalf("retention boundaries = %q .. %q", got[0].ID, got[len(got)-1].ID)
	}
	if _, err := store.DecisionByID(context.Background(), "event-0000"); !errors.Is(err, ErrDecisionNotFound) {
		t.Fatalf("old decision survived retention: %v", err)
	}
}

func TestRouteJournalBatchIdempotencyAndCalendarAggregation(t *testing.T) {
	store := testStore(t, 3)
	base := time.Date(2026, time.November, 24, 3, 10, 0, 0, time.UTC)
	batch := []RouteBucket{
		{Start: base.Add(10 * time.Second), Exit: "US", RegionSource: RouteRegionConfigured, Target: "DE", Chain: "warp", DownloadBytes: 8, Connections: 1},
		{Start: base.Add(40 * time.Minute), Exit: "US", RegionSource: RouteRegionConfigured, Target: "DE", Chain: "warp", UploadBytes: 3, Connections: 2},
		{Start: base, Exit: "", Target: "", Chain: "", DownloadBytes: 5},
	}
	if err := store.AddRouteBuckets(context.Background(), batch, "poll-1"); err != nil {
		t.Fatal(err)
	}
	replay := []RouteBucket{batch[2], batch[1], batch[0]}
	if err := store.AddRouteBuckets(context.Background(), replay, "poll-1"); err != nil {
		t.Fatalf("replayed poll: %v", err)
	}
	changed := append([]RouteBucket(nil), batch...)
	changed[0].DownloadBytes++
	if err := store.AddRouteBuckets(context.Background(), changed, "poll-1"); err == nil {
		t.Fatal("accepted reused poll id with different counters")
	}
	hourly, err := store.RouteBuckets(context.Background(), base.Truncate(time.Hour), base.Add(time.Hour), RouteHour)
	if err != nil || len(hourly) != 2 {
		t.Fatalf("hourly buckets = %#v, %v", hourly, err)
	}
	var known, unknown RouteBucket
	for _, bucket := range hourly {
		if bucket.Exit == "US" {
			known = bucket
		} else {
			unknown = bucket
		}
	}
	if known.DownloadBytes != 8 || known.UploadBytes != 3 || known.Connections != 3 || known.RegionSource != RouteRegionConfigured {
		t.Fatalf("known route = %#v", known)
	}
	if unknown.Exit != RouteUnknown || unknown.Target != RouteUnknown || unknown.Chain != RouteUnknown || unknown.DownloadBytes != 5 {
		t.Fatalf("unknown route = %#v", unknown)
	}
	period, err := store.RouteBuckets(context.Background(), base, base.Add(time.Hour), RoutePeriod)
	if err != nil || len(period) != 2 || !period[0].Start.Equal(base) || !period[1].Start.Equal(base) {
		t.Fatalf("period routes = %#v, %v", period, err)
	}
}

func TestRouteJournalKeepsDistinctPhysicalEntries(t *testing.T) {
	store := testStore(t, 3)
	start := store.now().UTC().Truncate(time.Minute)
	buckets := []RouteBucket{
		{Start: start, Entry: "JP", Exit: "US", RegionSource: RouteRegionConfigured, Target: "DE", Chain: "warp", DownloadBytes: 4},
		{Start: start, Entry: "SG", Exit: "US", RegionSource: RouteRegionConfigured, Target: "DE", Chain: "warp", DownloadBytes: 6},
	}
	if err := store.AddRouteBuckets(context.Background(), buckets, "two-entries"); err != nil {
		t.Fatal(err)
	}
	got, err := store.RouteBuckets(context.Background(), start, start.Add(time.Minute), RouteMinute)
	if err != nil || len(got) != 2 || got[0].Entry == got[1].Entry || got[0].DownloadBytes+got[1].DownloadBytes != 10 {
		t.Fatalf("entry-separated routes = %#v, %v", got, err)
	}
}

func TestRouteJournalRollsBackFailedBatchAndAllowsRetry(t *testing.T) {
	store := testStore(t, 3)
	start := store.now().UTC().Truncate(time.Minute)
	key := RouteBucket{Start: start, Exit: "US", RegionSource: RouteRegionPeer, Target: "FR", Chain: "front", DownloadBytes: math.MaxInt64}
	if err := store.AddRouteBucket(context.Background(), key, "poll-1"); err != nil {
		t.Fatal(err)
	}
	overflow := key
	overflow.DownloadBytes = 1
	if err := store.AddRouteBucket(context.Background(), overflow, "poll-2"); err == nil {
		t.Fatal("accepted overflowing route counter")
	}
	retry := key
	retry.DownloadBytes = 0
	retry.UploadBytes = 1
	if err := store.AddRouteBucket(context.Background(), retry, "poll-2"); err != nil {
		t.Fatalf("rolled-back source id could not retry: %v", err)
	}
	got, err := store.RouteBuckets(context.Background(), start, start.Add(time.Minute), RouteMinute)
	if err != nil || len(got) != 1 || got[0].DownloadBytes != math.MaxInt64 || got[0].UploadBytes != 1 {
		t.Fatalf("route after retry = %#v, %v", got, err)
	}
}

func TestRouteJournalRetentionDropsExpiredBuckets(t *testing.T) {
	store := testStore(t, 3)
	now := store.now().UTC().Truncate(time.Minute)
	buckets := []RouteBucket{
		{Start: now.Add(-routeRetention - time.Minute), Exit: "US", Target: "DE", Chain: "old", DownloadBytes: 7},
		{Start: now, Exit: "JP", Target: "US", Chain: "current", DownloadBytes: 9},
	}
	if err := store.AddRouteBuckets(context.Background(), buckets, "retention-poll"); err != nil {
		t.Fatal(err)
	}
	got, err := store.RouteBuckets(context.Background(), time.Time{}, time.Time{}, RouteMinute)
	if err != nil || len(got) != 1 || got[0].Chain != "current" || got[0].DownloadBytes != 9 {
		t.Fatalf("retained routes = %#v, %v", got, err)
	}
}

func TestRouteJournalConcurrentWritersPreserveEveryDelta(t *testing.T) {
	store := testStore(t, 3)
	start := store.now().UTC().Truncate(time.Minute)
	const writers = 16
	var wg sync.WaitGroup
	for index := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bucket := RouteBucket{Start: start, Exit: "JP", RegionSource: RouteRegionPeer, Target: "US", Chain: "primary", DownloadBytes: 1, Connections: 1}
			if err := store.AddRouteBucket(context.Background(), bucket, "poll-"+string(rune('a'+index))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := store.RouteBuckets(context.Background(), start, start.Add(time.Minute), RouteMinute)
	if err != nil || len(got) != 1 || got[0].DownloadBytes != writers || got[0].Connections != writers {
		t.Fatalf("concurrent route = %#v, %v", got, err)
	}
}

func TestAnalyticsRejectsCorruptDatabaseAndCancelledWrites(t *testing.T) {
	store := testStore(t, 3)
	if err := store.ensureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.analyticsPath(), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecentDecisions(context.Background(), 10); err == nil {
		t.Fatal("corrupt analytics database was accepted")
	}
	if err := os.Remove(store.analyticsPath()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decision := Decision{ID: "cancelled", At: time.Now(), Source: "test", Group: "g", Action: "kept"}
	if err := store.RecordDecision(ctx, decision); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write error = %v", err)
	}
	if _, err := os.Stat(store.analyticsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled write created database: %v", err)
	}
}

func TestAnalyticsRejectsUnknownSchemaVersion(t *testing.T) {
	store := testStore(t, 3)
	decision := Decision{ID: "known", At: store.now(), Source: "test", Group: "g", Action: "kept"}
	if err := store.RecordDecision(context.Background(), decision); err != nil {
		t.Fatal(err)
	}
	db, _, err := store.openAnalytics(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := execSQL(db, "PRAGMA user_version = 99"); err != nil {
		closeAnalytics(db)
		t.Fatal(err)
	}
	closeAnalytics(db)
	decision.ID = "rejected"
	if err := store.RecordDecision(context.Background(), decision); err == nil {
		t.Fatal("write accepted an unknown analytics schema")
	}
	if _, err := store.RecentDecisions(context.Background(), 10); err == nil {
		t.Fatal("read accepted an unknown analytics schema")
	}
}
