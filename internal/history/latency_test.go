package history

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestLatencyHistoryRoundBaselineAndIsolation(t *testing.T) {
	store := testStore(t, 10)
	at := store.now().UTC()
	round := []model.Sample{
		latencySample(at, "one", 100, 2, 0, ""),
		latencySample(at.Add(time.Second), "two", 200, 2, 1, "download interrupted"),
		latencySample(at.Add(2*time.Second), "zero", 0, 2, 0, ""),
		latencySample(at.Add(3*time.Second), "probe-failed", 0, 1, 1, "204 probe: HTTP 504"),
	}
	if err := store.RecordLatencyRound(context.Background(), "group-a", "probe-v1", round); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordLatencyRound(context.Background(), "group-a", "probe-v1", reverseSamples(round)); err != nil {
		t.Fatal(err)
	}
	got, err := store.LatencyBaseline("group-a", "probe-v1")
	if err != nil || got.Samples != 3 || got.MeanMS != 100 {
		t.Fatalf("baseline = %+v, %v", got, err)
	}
	for _, key := range [][2]string{{"group-b", "probe-v1"}, {"group-a", "probe-v2"}} {
		got, err = store.LatencyBaseline(key[0], key[1])
		if err != nil || got != (LatencyBaseline{}) {
			t.Fatalf("isolated baseline %v = %+v, %v", key, got, err)
		}
	}
	info, err := os.Stat(store.latencyPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("latency sidecar mode = %v", info.Mode().Perm())
	}

	reopened := New(store.file, 10, 1)
	got, err = reopened.LatencyBaseline("group-a", "probe-v1")
	if err != nil || got.Samples != 3 || got.MeanMS != 100 {
		t.Fatalf("reopened baseline = %+v, %v", got, err)
	}
}

func TestLatencyHistoryRetainsNewestRoundsByObservationTime(t *testing.T) {
	store := testStore(t, 10)
	base := store.now().UTC()
	for index := 24; index >= 0; index-- {
		sample := latencySample(base.Add(time.Duration(index)*time.Minute), "node", index, 2, 0, "")
		if err := store.RecordLatencyRound(context.Background(), "group", "probe", []model.Sample{sample}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.LatencyBaseline("group", "probe")
	// Newest observed rounds are 5..24, independent of write order.
	if err != nil || got.Samples != 20 || got.MeanMS != 14.5 {
		t.Fatalf("retained baseline = %+v, %v", got, err)
	}
}

func TestLatencyHistoryConcurrentRoundsAreMerged(t *testing.T) {
	store := testStore(t, 10)
	base := store.now().UTC()
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < 12; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sample := latencySample(base.Add(time.Duration(index)*time.Second), "node", index+1, 2, 0, "")
			if err := store.RecordLatencyRound(ctx, "group", "probe", []model.Sample{sample}); err != nil {
				t.Errorf("record round %d: %v", index, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	got, err := store.LatencyBaseline("group", "probe")
	if err != nil || got.Samples != 12 || got.MeanMS != 6.5 {
		t.Fatalf("concurrent baseline = %+v, %v", got, err)
	}
}

func TestLatencyHistorySkipsUnknownAndBoundsProbeSettings(t *testing.T) {
	store := testStore(t, 10)
	base := store.now().UTC()
	unknown := latencySample(base, "unknown", 0, 1, 0, "")
	if err := store.RecordLatencyRound(context.Background(), "group", "probe", []model.Sample{unknown}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.latencyPath()); !os.IsNotExist(err) {
		t.Fatalf("empty round created sidecar: %v", err)
	}
	for index := 0; index < latencySeriesPerGroup+2; index++ {
		sample := latencySample(base.Add(time.Duration(index)*time.Minute), "node", 10+index, 2, 0, "")
		if err := store.RecordLatencyRound(context.Background(), "group", string(rune('a'+index)), []model.Sample{sample}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := store.readLatencyState()
	if err != nil || len(state.Groups["group"]) != latencySeriesPerGroup {
		t.Fatalf("retained probe settings = %d, %v", len(state.Groups["group"]), err)
	}
	if old, err := store.LatencyBaseline("group", "a"); err != nil || old.Samples != 0 {
		t.Fatalf("old probe baseline = %+v, %v", old, err)
	}
}

func latencySample(at time.Time, node string, latency, attempts, failures int, failure string) model.Sample {
	return model.Sample{At: at, Node: node, Scope: "front", Kind: "sweep", LatencyMS: latency,
		Attempts: attempts, Failures: failures, Error: failure}
}

func reverseSamples(samples []model.Sample) []model.Sample {
	result := append([]model.Sample(nil), samples...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}
