package history

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestSelectionTenureStartsWithObservationInMissingOrLegacyHistory(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			s := testStore(t, 2)
			at := s.now()
			if legacy {
				if err := s.ensureDir(); err != nil {
					t.Fatal(err)
				}
				raw := fmt.Sprintf(`{"version":1,"cycle_start":%q,"used_bytes":17,
					"samples":[{"time":%q,"node":"a","scope":"front","kind":"bench","download_mbps":20}],
					"heartbeats":{"primary":{"scope":"primary","attempts":10,"last_time":%q,"last_latency_ms":7}}}`,
					cycleStart(at, 1).Format(time.RFC3339), at.Add(-time.Hour).Format(time.RFC3339), at.Add(-time.Minute).Format(time.RFC3339))
				if err := os.WriteFile(s.file, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := recordTestSelection(t, s, "primary", "a", at, false)
			if got.Node != "a" || !got.Since.Equal(at) || !got.LastSeen.Equal(at) || got.GuardSelected || len(got.Longest) != 0 {
				t.Fatalf("first observation backfilled a span: %+v", got)
			}
			snapshot, err := s.Snapshot(context.Background())
			if err != nil || !reflect.DeepEqual(got, snapshot.Selections["primary"]) {
				t.Fatalf("durable selection = %+v, %v", snapshot.Selections, err)
			}
			if legacy && (snapshot.UsedBytes != 17 || len(snapshot.Samples) != 0 || len(snapshot.Heartbeats) != 1) {
				t.Fatalf("legacy download cleanup changed accounting or heartbeat history: %+v", snapshot)
			}
		})
	}
}

func TestSelectionTenureAcrossRestartKeepManualChangeAndBillingRollover(t *testing.T) {
	s := testStore(t, 2)
	december := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.Local)
	start := december.Add(-time.Hour)
	s.now = func() time.Time { return start }
	recordTestSelection(t, s, "primary", "a", start, true)
	recordTestSelection(t, s, "other", "a", start, false)
	s = New(s.file, 2, 1)
	s.now = func() time.Time { return december }
	a := recordTestSelection(t, s, "primary", "a", december, false)
	if !a.Since.Equal(start) || !a.GuardSelected || a.Longest["a"] != time.Hour {
		t.Fatalf("keep after restart or rollover reset history: %+v", a)
	}
	// The change occurred somewhere in the unobserved gap. Neither member
	// gets credited with that interval.
	manual := december.Add(5 * time.Hour)
	b := recordTestSelection(t, s, "primary", "b", manual, false)
	if !b.Since.Equal(manual) || b.GuardSelected || b.Longest["b"] != 0 || b.Longest["a"] != time.Hour {
		t.Fatalf("manual observation = %+v", b)
	}
	recordTestSelection(t, s, "primary", "b", manual.Add(2*time.Hour), false)
	back := manual.Add(3 * time.Hour)
	a = recordTestSelection(t, s, "primary", "a", back, true)
	if !a.GuardSelected || !a.Since.Equal(back) || a.Longest["b"] != 2*time.Hour || a.Longest["a"] != time.Hour {
		t.Fatalf("confirmed switch = %+v", a)
	}
	// A confirmed change to the stored member can follow a missed manual
	// departure while the daemon was stopped.
	a = recordTestSelection(t, s, "primary", "a", back.Add(time.Minute), true)
	if !a.Since.Equal(back.Add(time.Minute)) || a.Longest["a"] != time.Hour {
		t.Fatalf("confirmed new stint merged with earlier stint: %+v", a)
	}
	snapshot, err := s.Snapshot(context.Background())
	if err != nil || !snapshot.CycleStart.Equal(december) || !reflect.DeepEqual(snapshot.Selections["primary"], a) ||
		!snapshot.Selections["other"].LastSeen.Equal(start) || len(snapshot.Samples) != 0 {
		t.Fatalf("snapshot after rollover = %+v, %v", snapshot, err)
	}
}

func TestSelectionTenureRejectsOutOfOrderAndDuplicateEvents(t *testing.T) {
	s := testStore(t, 2)
	base := s.now()
	recordTestSelection(t, s, "p", "a", base, false)
	at := base.Add(time.Minute)
	recordTestSelection(t, s, "p", "b", at, false)
	want := recordTestSelection(t, s, "p", "b", at, true)
	if !want.GuardSelected {
		t.Fatal("confirmation lost to observation at the same time")
	}
	for _, delayed := range []struct {
		node     string
		at       time.Time
		switched bool
	}{
		{"a", base, false}, {"a", base, true}, {"a", at, false}, {"b", at, true},
	} {
		got := recordTestSelection(t, s, "p", delayed.node, delayed.at, delayed.switched)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("late/replayed event changed durable winner: %+v", got)
		}
	}
}

func TestSelectionTenureConcurrentStoresPreserveLatestAndBudget(t *testing.T) {
	s := testStore(t, 2)
	base := s.now()
	other := New(s.file, 2, 1)
	other.now = s.now
	ctx := context.Background()
	reservation, err := s.Reserve(ctx, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := other.RecordSelection(ctx, "p", fmt.Sprint(i), base.Add(time.Duration(i)*time.Minute), i == 7)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	if err := s.Finish(ctx, reservation, model.Sample{Node: "leaf", Bytes: 10}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	snapshot, err := s.Snapshot(ctx)
	winner := snapshot.Selections["p"]
	if err != nil || winner.Node != "7" || !winner.GuardSelected || !winner.Since.Equal(base.Add(7*time.Minute)) ||
		snapshot.UsedBytes != 10 || snapshot.PendingBytes != 0 || len(snapshot.Samples) != 1 {
		t.Fatalf("concurrent writes: %+v, %v", snapshot, err)
	}
}

func TestMalformedSelectionCannotInventTenure(t *testing.T) {
	s := testStore(t, 2)
	at := s.now()
	for _, tc := range []struct {
		group, node string
		at          time.Time
	}{{"", "a", at}, {"p", "", at}, {"p", "a", time.Time{}}} {
		if _, err := s.RecordSelection(context.Background(), tc.group, tc.node, tc.at, false); err == nil {
			t.Fatal("accepted incomplete selection", tc)
		}
	}
	selections := map[string]SelectionTenure{"p": {Node: "a", LastSeen: at}}
	if err := s.writeHistoryJSON(s.file+".selections.json", selections); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RecordSelection(context.Background(), "p", "a", at, false); err == nil || got.Node != "" {
		t.Fatalf("missing start created a span: %+v, %v", got, err)
	}
}

func TestRecordingSelectionLeavesLegacyBandwidthLedgerIntact(t *testing.T) {
	s := testStore(t, 2)
	if err := s.Record(context.Background(), model.Sample{Node: "a", Kind: "bench", ProxyType: "Vless", Mbps: 5}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	recordTestSelection(t, s, "primary", "a", s.now(), true)
	after, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("recording a switch changed the v0.1.x bandwidth ledger")
	}
	if strings.Contains(string(after), `"proxy_type"`) {
		t.Fatal("old daemons cannot decode an added sample field")
	}
}

func recordTestSelection(t *testing.T, s *Store, group, node string, at time.Time, switched bool) SelectionTenure {
	t.Helper()
	got, err := s.RecordSelection(context.Background(), group, node, at, switched)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
