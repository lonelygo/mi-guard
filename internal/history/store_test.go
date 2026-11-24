package history

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestDownloadProfilesSurviveLedgerRewritesAlongsideLegacySamples(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, 4)
	old := measurement("a", 12, 128)
	if err := store.Record(ctx, old); err != nil {
		t.Fatal(err)
	}
	sample := measurement("a", 29, 192)
	sample.Kind, sample.Scope = "bench", "chain:a/path"
	sample.Profile = model.DownloadProfile{Streams: 4, TotalBytes: 512, Target: strings.Repeat("ab", 32)}
	res, err := store.Reserve(ctx, 512, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, res, sample); err != nil {
		t.Fatal(err)
	}
	// The strict v1 reader sees two ordinary sample records and preserves their
	// scope strings through a read/write cycle.
	raw, _, err := store.load()
	if err != nil || len(raw.Samples) != 2 {
		t.Fatalf("v1 ledger: %+v, %v", raw, err)
	}
	if raw.Samples[0].Scope != old.Scope || raw.Samples[1].Scope != downloadScopePrefix+"4/512/"+sample.Profile.Target+"/"+sample.Scope {
		t.Fatalf("stored scopes = %q, %q", raw.Samples[0].Scope, raw.Samples[1].Scope)
	}
	if err := store.write(raw); err != nil {
		t.Fatal(err)
	}
	view, err := store.Snapshot(ctx)
	if err != nil || view.PendingBytes != 0 || view.UsedBytes != sample.Bytes || len(view.Samples) != 2 {
		t.Fatalf("snapshot = %+v, %v", view, err)
	}
	if legacy, measured := view.Samples[0], view.Samples[1]; legacy.Profile.Known() || legacy.Scope != "front" || measured.Profile != sample.Profile || measured.Scope != sample.Scope {
		t.Fatalf("decoded measurements = %+v", view.Samples)
	}
	for _, scope := range []string{"front", "download/v1/4/16/bad/front", "download/v1/9/512/" + sample.Profile.Target + "/front"} {
		if route, profile := readDownloadScope(scope); profile.Known() || route != scope {
			t.Errorf("unlabelled scope %q decoded as %q, %+v", scope, route, profile)
		}
	}
}

func TestReserveFinishAndIdempotence(t *testing.T) {
	store := testStore(t, 2)
	ctx := context.Background()

	first, err := store.Reserve(ctx, 100, 150)
	if err != nil || first.Bytes != 100 {
		t.Fatalf("first reservation = %+v, %v", first, err)
	}
	second, err := store.Reserve(ctx, 100, 150)
	if err != nil || second.Bytes != 50 {
		t.Fatalf("bounded reservation = %+v, %v", second, err)
	}
	if _, err := store.Reserve(ctx, 1, 150); !errors.Is(err, ErrBudget) {
		t.Fatalf("exhausted reserve error = %v", err)
	}

	sample := measurement("one", 10, 60)
	if err := store.Finish(ctx, first, sample); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, first, sample); err != nil {
		t.Fatalf("duplicate finish: %v", err)
	}
	if err := store.Finish(ctx, second, measurement("two", 20, 40)); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UsedBytes != 100 || snapshot.PendingBytes != 0 {
		t.Fatalf("usage = %d, pending = %d", snapshot.UsedBytes, snapshot.PendingBytes)
	}
	if len(snapshot.Samples) != 2 {
		t.Fatalf("sample count = %d, want 2", len(snapshot.Samples))
	}
	info, err := os.Stat(store.file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("history mode = %o", info.Mode().Perm())
	}
}

func TestRetentionDoesNotEraseCycleUsage(t *testing.T) {
	store := testStore(t, 2)
	ctx := context.Background()
	for index := 0; index < 4; index++ {
		reservation, err := store.Reserve(ctx, 10, 100)
		if err != nil {
			t.Fatal(err)
		}
		sample := measurement("same", float64(index+1), 10)
		sample.At = store.now().Add(time.Duration(index) * time.Minute)
		if err := store.Finish(ctx, reservation, sample); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Samples) != 2 {
		t.Fatalf("retained %d samples, want 2", len(snapshot.Samples))
	}
	if snapshot.UsedBytes != 40 {
		t.Fatalf("usage after truncation = %d, want 40", snapshot.UsedBytes)
	}
}

func TestHeartbeatIsAggregatedOutsideBandwidthWindow(t *testing.T) {
	store := testStore(t, 1)
	ctx := context.Background()
	for _, sample := range []model.Sample{
		{Kind: "heartbeat", Scope: "US", Attempts: 1, LatencyMS: 10},
		{Kind: "heartbeat", Scope: "US", Attempts: 1, Failures: 1, Error: "timeout", LatencyMS: 20},
	} {
		if err := store.Record(ctx, sample); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Samples) != 0 || len(snapshot.Heartbeats) != 1 {
		t.Fatalf("samples=%d heartbeats=%d", len(snapshot.Samples), len(snapshot.Heartbeats))
	}
	got := snapshot.Heartbeats[0]
	if got.Attempts != 2 || got.Failures != 1 || got.LastError != "timeout" {
		t.Fatalf("heartbeat aggregate = %+v", got)
	}
}

func TestMarkSweepPersistsCadenceWithoutSyntheticSample(t *testing.T) {
	store := testStore(t, 5)
	at := time.Date(2026, time.November, 20, 18, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	if err := store.MarkSweep(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	reopened := New(store.file, 5, 1)
	reopened.now = store.now
	snapshot, err := reopened.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.LastSweep.Equal(at) || len(snapshot.Samples) != 0 {
		t.Fatalf("sweep snapshot = %+v", snapshot)
	}

	rollover := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.Local)
	reopened.now = func() time.Time { return rollover }
	snapshot, err = reopened.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.LastSweep.Equal(at) || len(snapshot.Samples) != 0 {
		t.Fatalf("sweep after rollover = %+v", snapshot)
	}
	if err := reopened.MarkSweep(context.Background(), time.Time{}); err == nil {
		t.Fatal("zero sweep time was accepted")
	}
}

func TestReadOnlySnapshotUsesLocalBillingCalendarWithoutWritingRollover(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	s := testStore(t, 5)
	s.billingDay = 15
	now := time.Date(2026, time.March, 20, 12, 0, 0, 0, london)
	s.now = func() time.Time { return now }
	state := newDiskState(cycleStart(now, s.billingDay))
	state.UsedBytes = 17
	state.Reservations["unfinished"] = diskReservation{Bytes: 5, CreatedAt: now}
	state.LastSweep = now
	state.Samples = []model.Sample{{At: now, Node: "node", Scope: "front", Kind: "bench", Mbps: 12, Bytes: 7}}
	if err := s.write(state); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.file)
	if err != nil {
		t.Fatal(err)
	}
	nextCycle := time.Date(2026, time.April, 15, 0, 0, 0, 0, london)
	current, err := s.ReadOnlySnapshot()
	if err != nil || current.UsedBytes != 17 || current.PendingBytes != 5 || !current.CycleStart.AddDate(0, 1, 0).Equal(nextCycle) {
		t.Fatalf("current cycle including DST change: %+v, %v", current, err)
	}
	now = nextCycle
	rolled, err := s.ReadOnlySnapshot()
	if err != nil || !rolled.CycleStart.Equal(nextCycle) || rolled.UsedBytes != 0 || rolled.PendingBytes != 0 ||
		!rolled.LastSweep.Equal(state.LastSweep) || len(rolled.Samples) != 1 {
		t.Fatalf("new cycle: %+v, %v", rolled, err)
	}
	after, err := os.Stat(s.file)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("read replaced or modified the ledger: %v", err)
	}
	persisted, _, err := s.load()
	if err != nil || !persisted.CycleStart.Equal(state.CycleStart) || persisted.UsedBytes != 17 || persisted.Previous != nil {
		t.Fatalf("rollover was persisted: %+v, %v", persisted, err)
	}
	entries, err := os.ReadDir(filepath.Dir(s.file))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(s.file) {
		t.Fatalf("unexpected sidecar or lock: %v, %v", entries, err)
	}
}

func TestBillingCycleRolloverSettlesOriginalCycleWithoutTouchingNewBudget(t *testing.T) {
	store := testStore(t, 5)
	now := time.Date(2026, time.January, 20, 9, 0, 0, 0, time.Local)
	store.now = func() time.Time { return now }
	reservation, err := store.Reserve(context.Background(), 80, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), measurement("node", 10, 0)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, time.February, 15, 0, 0, 0, 0, time.Local)
	newReservation, err := store.Reserve(context.Background(), 90, 100)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UsedBytes != 90 || snapshot.PendingBytes != 90 || len(snapshot.Samples) != 1 {
		t.Fatalf("after rollover: %+v", snapshot)
	}
	if err := store.Finish(context.Background(), reservation, measurement("node", 1, 1)); err != nil {
		t.Fatalf("late finish: %v", err)
	}
	if err := store.Finish(context.Background(), reservation, measurement("node", 1, 1)); err != nil {
		t.Fatalf("duplicate late finish: %v", err)
	}
	snapshot, err = store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UsedBytes != 90 || snapshot.PendingBytes != 90 || len(snapshot.Samples) != 2 {
		t.Fatalf("after late finish: %+v", snapshot)
	}
	state, _, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Previous == nil || state.Previous.UsedBytes != 1 || len(state.Previous.Reservations) != 0 {
		t.Fatalf("previous ledger = %+v", state.Previous)
	}
	last, err := store.Reserve(context.Background(), 20, 100)
	if err != nil || last.Bytes != 10 {
		t.Fatalf("new-cycle remaining reservation = %+v, %v", last, err)
	}
	if err := store.Finish(context.Background(), newReservation, measurement("new", 2, 90)); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedCurrentCycleBudgetFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*diskState)
	}{
		{name: "zero cycle", mutate: func(state *diskState) { state.CycleStart = time.Time{} }},
		{name: "negative used bytes", mutate: func(state *diskState) { state.UsedBytes = -1 }},
		{name: "pending exceeds used", mutate: func(state *diskState) {
			state.Reservations["broken"] = diskReservation{Bytes: 2, CreatedAt: time.Now()}
			state.UsedBytes = 1
		}},
		{name: "malformed cycle boundary", mutate: func(state *diskState) { state.CycleStart = state.CycleStart.Add(time.Hour) }},
		{name: "future cycle", mutate: func(state *diskState) { state.CycleStart = state.CycleStart.AddDate(0, 1, 0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t, 5)
			state := newDiskState(cycleStart(store.now(), store.billingDay))
			test.mutate(&state)
			if err := store.write(state); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Snapshot(context.Background()); err == nil {
				t.Fatal("damaged budget was accepted")
			}
			if _, err := store.ReadOnlySnapshot(); err == nil {
				t.Fatal("dashboard accepted a damaged budget")
			}
			after, err := os.ReadFile(store.file)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("damaged budget was silently rewritten")
			}
		})
	}
}

func TestCorruptHistoryIsNotSilentlyReset(t *testing.T) {
	store := testStore(t, 5)
	contents := []byte("{broken")
	if err := store.ensureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.file, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot accepted corrupt history")
	}
	after, err := os.ReadFile(store.file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(contents) {
		t.Fatalf("corrupt file was overwritten: %q", after)
	}
}

func TestWithLockHonorsContext(t *testing.T) {
	store := testStore(t, 5)
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithLock(context.Background(), "measure", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := store.WithLock(ctx, "measure", func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWithLockIsCrossProcessAndNamesAreIndependent(t *testing.T) {
	store := testStore(t, 5)
	ready := store.file + ".ready"
	release := store.file + ".release"
	command := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	command.Env = append(os.Environ(),
		"MI_GUARD_LOCK_HELPER=1",
		"MI_GUARD_LOCK_FILE="+store.file,
		"MI_GUARD_LOCK_READY="+ready,
		"MI_GUARD_LOCK_RELEASE="+release,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = os.WriteFile(release, []byte("release"), 0o600)
		if !waited {
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := store.WithLock(ctx, "measure", func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock error = %v", err)
	}
	if err := store.WithLock(context.Background(), "apply", func() error { return nil }); err != nil {
		t.Fatalf("independent lock name was blocked: %v", err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
}

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv("MI_GUARD_LOCK_HELPER") != "1" {
		t.Skip("helper process")
	}
	store := New(os.Getenv("MI_GUARD_LOCK_FILE"), 1, 1)
	err := store.WithLock(context.Background(), "measure", func() error {
		if err := os.WriteFile(os.Getenv("MI_GUARD_LOCK_READY"), []byte("ready"), 0o600); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(os.Getenv("MI_GUARD_LOCK_RELEASE")); err == nil {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReserveCannotExceedBudget(t *testing.T) {
	store := testStore(t, 5)
	const attempts = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := int64(0)
	unexpected := make([]error, 0)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := store.Reserve(context.Background(), 10, 100)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				granted += reservation.Bytes
			} else if !errors.Is(err, ErrBudget) {
				unexpected = append(unexpected, err)
			}
		}()
	}
	wg.Wait()
	if len(unexpected) != 0 {
		t.Fatalf("unexpected errors: %v", unexpected)
	}
	if granted != 100 {
		t.Fatalf("granted %d bytes, want 100", granted)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UsedBytes != 100 || snapshot.PendingBytes != 100 {
		t.Fatalf("concurrent snapshot = %+v", snapshot)
	}
}

func testStore(t *testing.T, window int) *Store {
	t.Helper()
	store := New(filepath.Join(t.TempDir(), "state", "speed-history.json"), window, 1)
	now := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.Local)
	store.now = func() time.Time { return now }
	return store
}

func measurement(node string, mbps float64, bytes int64) model.Sample {
	return model.Sample{
		At:       time.Date(2026, time.November, 20, 12, 0, 0, 0, time.Local),
		Node:     node,
		Provider: "provider",
		Scope:    "front",
		Kind:     "sweep",
		Mbps:     mbps,
		Bytes:    bytes,
	}
}

func TestDiscardAccountsBytesWithoutDamagingNodeHistory(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "history.json"), 1, 1)
	ctx := context.Background()
	r, err := store.Reserve(ctx, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Discard(ctx, r, 37); err != nil {
		t.Fatal(err)
	}
	if err = store.Discard(ctx, r, 37); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.UsedBytes != 37 || len(s.Samples) != 0 {
		t.Fatalf("unattributed data entered history: %+v", s)
	}
}
