package guard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

func TestHeartbeatHardFailureRequiresTwoChecksAndBypassesPerformanceCooldown(t *testing.T) {
	fixture, server := newControllerFixture(t)
	fixture.failChainOnNode = "a"
	runner := testRunner(t, fixture, server)
	runner.Cfg.Groups = []config.Group{{Name: "warp", Chain: "chain", Direct: "direct"}}
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{"warp": {current: "a", cooldownUntil: time.Now().Add(time.Hour)}}}

	if err := runner.Store.Record(context.Background(), model.Sample{At: time.Now(), Node: "b", Provider: "airport", Scope: "front", Kind: "sweep", Mbps: 80, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.heartbeat(context.Background(), runner.Cfg.Groups[0]); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	if len(fixture.writes) != 0 {
		t.Fatalf("first failure switched: %v", fixture.writes)
	}
	fixture.mu.Unlock()

	if err := runtime.heartbeat(context.Background(), runner.Cfg.Groups[0]); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" || fixture.proxies["direct"].Now != "b" {
		t.Fatalf("recovery did not switch both selectors: warp=%q direct=%q", fixture.proxies["warp"].Now, fixture.proxies["direct"].Now)
	}
}

func TestStandaloneHeartbeatProbesSelectorAndRecoversWithoutChainState(t *testing.T) {
	fixture, server := newControllerFixture(t)
	fixture.failOnSelection["warp"] = "a"
	runner := testRunner(t, fixture, server)
	group := config.Group{Name: "warp", Mode: config.GroupModeSelector}
	runner.Cfg.Groups = []config.Group{group}
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{"warp": {current: "a"}}}

	if err := runner.Store.Record(context.Background(), model.Sample{At: time.Now(), Node: "b", Provider: "airport", Scope: "front", Kind: "sweep", Mbps: 80, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.heartbeat(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	if err := runtime.heartbeat(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	selected := fixture.proxies["warp"].Now
	connectionReads := fixture.connectionReads
	delayNames := append([]string(nil), fixture.delayNames...)
	fixture.mu.Unlock()
	if selected != "b" {
		t.Fatalf("standalone recovery selected %q", selected)
	}
	if connectionReads != 0 {
		t.Fatalf("standalone daemon read connections %d times", connectionReads)
	}
	for _, name := range delayNames {
		if name == "" || name == "chain" {
			t.Fatalf("standalone daemon probed %q: %v", name, delayNames)
		}
	}

	snapshot, err := runner.Store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Heartbeats) != 1 || snapshot.Heartbeats[0].Scope != "warp" || snapshot.Heartbeats[0].Attempts != 2 {
		t.Fatalf("standalone heartbeat aggregate = %+v", snapshot.Heartbeats)
	}
}

func TestScheduleAndCandidateHelpers(t *testing.T) {
	now := time.Now()
	samples := []model.Sample{
		{At: now.Add(-2 * time.Hour), Node: "a", Kind: "sweep", Scope: "front", Mbps: 100},
		{At: now.Add(-time.Hour), Node: "a", Kind: "sweep", Scope: "front", Mbps: 90},
		{At: now.Add(-3 * time.Hour), Node: "b", Kind: "sweep", Scope: "front", Mbps: 120},
	}
	ranked := historicCandidates("a", []string{"a", "b", "c"}, samples)
	if len(ranked) != 3 {
		t.Fatalf("historic candidate count %d", len(ranked))
	}
	if ranked[len(ranked)-1].Node != "c" || ranked[len(ranked)-1].Mbps != 0 {
		t.Fatalf("unknown candidate was turned into a fake observation: %#v", ranked)
	}
}

func TestFullSweepUsesOneDurableRoundCadence(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Schedule.FullInterval = 12 * time.Hour
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{}}

	if _, due := runtime.nextFullScan(context.Background()); !due {
		t.Fatal("empty history did not schedule an initial full sweep")
	}
	if err := runner.Store.MarkSweep(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runner.Store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastSweep.IsZero() || len(snapshot.Samples) != 0 {
		t.Fatalf("cadence metadata polluted samples: last=%v samples=%d", snapshot.LastSweep, len(snapshot.Samples))
	}
	delay, due := runtime.nextFullScan(context.Background())
	if due || delay < 11*time.Hour {
		t.Fatalf("recent complete/partial round rescheduled early: due=%v delay=%v", due, delay)
	}
}

func TestSlowSchedulerBoundaryConfirmationAndVerifiedAlternative(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.NearBestPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	runner.Cfg.Speed.ConfirmBytes = 1024
	group := config.Group{Name: "warp", Chain: "chain"}
	past := []model.Sample{
		{At: time.Now(), Node: "a", Provider: "airport", Scope: "front", Kind: "sweep", Mbps: 100, Attempts: 1},
		{At: time.Now(), Node: "a", Provider: "airport", Scope: "front", Kind: "sweep", Mbps: 100, Attempts: 1},
		{At: time.Now(), Node: "a", Provider: "airport", Scope: "front", Kind: "sweep", Mbps: 100, Attempts: 1},
		{At: time.Now(), Node: "b", Provider: "airport", Scope: "front", Kind: "sweep", Mbps: 100, Attempts: 1},
	}
	initial := []model.Sample{{At: time.Now(), Node: "a", Provider: "airport", Scope: "front", Kind: "quick", Mbps: 80, Attempts: 1}}

	newRuntime := func(results []model.Sample) *daemonRuntime {
		var mu sync.Mutex
		index := 0
		runtime := &daemonRuntime{
			runner: runner,
			groups: map[string]*groupRuntime{"warp": {current: "a"}},
			candidatesFn: func(context.Context, string) ([]string, error) {
				return []string{"a", "b"}, nil
			},
		}
		runtime.measureFn = func(_ context.Context, node, kind string, _ int64) (model.Sample, error) {
			mu.Lock()
			defer mu.Unlock()
			if index >= len(results) {
				t.Fatalf("unexpected measurement of %s (%s)", node, kind)
			}
			result := results[index]
			index++
			if result.Node != node {
				t.Fatalf("measured %s, queued %s", node, result.Node)
			}
			return result, nil
		}
		return runtime
	}

	failedConfirmation := newRuntime([]model.Sample{{At: time.Now(), Node: "a", Provider: "airport", Scope: "front", Kind: "confirm", Mbps: 81, Attempts: 1}})
	failedConfirmation.considerSlow(context.Background(), group, "a", initial, past)
	failedConfirmation.wg.Wait()
	fixture.mu.Lock()
	if len(fixture.writes) != 0 {
		fixture.mu.Unlock()
		t.Fatalf("recovered confirmation switched: %v", fixture.writes)
	}
	fixture.mu.Unlock()

	confirmed := newRuntime([]model.Sample{
		{At: time.Now(), Node: "a", Provider: "airport", Scope: "front", Kind: "confirm", Mbps: 79, Attempts: 1},
		{At: time.Now(), Node: "b", Provider: "airport", Scope: "front", Kind: "confirm", Mbps: 100, Attempts: 1},
	})
	confirmed.considerSlow(context.Background(), group, "a", initial, past)
	confirmed.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" {
		t.Fatalf("confirmed boundary slowdown did not select verified alternative: %q", fixture.proxies["warp"].Now)
	}
}

func TestSlowSchedulerRemeasuresHistoricalAlternative(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.NearBestPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	runner.Cfg.Speed.ConfirmBytes = 1024
	group := config.Group{Name: "warp", Chain: "chain"}
	past := []model.Sample{
		{At: time.Now(), Node: "a", Scope: "front", Kind: "sweep", Mbps: 100},
		{At: time.Now(), Node: "b", Scope: "front", Kind: "sweep", Mbps: 120},
	}
	results := []model.Sample{
		{At: time.Now(), Node: "a", Scope: "front", Kind: "confirm", Mbps: 79, Attempts: 1},
		{At: time.Now(), Node: "b", Scope: "front", Kind: "confirm", Mbps: 50, Attempts: 1},
	}
	index := 0
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		candidatesFn: func(context.Context, string) ([]string, error) {
			return []string{"a", "b"}, nil
		},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			if index >= len(results) || results[index].Node != node {
				t.Fatalf("unexpected measurement %q at %d", node, index)
			}
			result := results[index]
			index++
			return result, nil
		},
	}
	runtime.considerSlow(context.Background(), group, "a", []model.Sample{{Node: "a", Scope: "front", Kind: "quick", Mbps: 80}}, past)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if index != 2 {
		t.Fatalf("historical alternative was not remeasured, calls=%d", index)
	}
	if len(fixture.writes) != 0 {
		t.Fatalf("stale historical speed caused a switch: %v", fixture.writes)
	}
}

func TestRepeatedAttributedDownloadFailureCanRecoverWithoutBaseline(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	results := []model.Sample{
		{At: time.Now(), Node: "a", Scope: "front", Kind: "confirm", Error: "connection reset", Attempts: 1, Failures: 1},
		{At: time.Now(), Node: "b", Scope: "front", Kind: "confirm", Mbps: 90, Attempts: 1},
	}
	index := 0
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		candidatesFn: func(context.Context, string) ([]string, error) {
			return []string{"a", "b"}, nil
		},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			if index >= len(results) || results[index].Node != node {
				t.Fatalf("unexpected measurement %q at %d", node, index)
			}
			result := results[index]
			index++
			return result, nil
		},
	}
	initial := []model.Sample{{At: time.Now(), Node: "a", Scope: "front", Kind: "quick", Error: "zero payload", Attempts: 1, Failures: 1}}
	runtime.considerSlow(context.Background(), group, "a", initial, nil)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" {
		t.Fatalf("two attributed failures did not recover to verified candidate: %q", fixture.proxies["warp"].Now)
	}
}

func TestAttributedDownloadFailureStopsWhenConfirmationRecovers(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	measurements := 0
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			measurements++
			return model.Sample{At: time.Now(), Node: node, Scope: "front", Kind: "confirm", Mbps: 80, Attempts: 1}, nil
		},
	}
	initial := []model.Sample{{At: time.Now(), Node: "a", Scope: "front", Kind: "quick", Error: "connection reset", Attempts: 1, Failures: 1}}
	runtime.considerSlow(context.Background(), group, "a", initial, nil)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if measurements != 1 || len(fixture.writes) != 0 {
		t.Fatalf("recovered confirmation measurements=%d writes=%v", measurements, fixture.writes)
	}
}

func TestSystemicMeasurementErrorDoesNotStartDegradationConfirmation(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Speed.QuickBytes = 1024
	measurements := 0
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			measurements++
			return model.Sample{Node: node, Error: probe.ErrRoute.Error()}, probe.ErrRoute
		},
	}
	runtime.runActiveCheck(context.Background(), config.Group{Name: "warp", Chain: "chain"})
	if measurements != 1 || runtime.groups["warp"].pendingSlow {
		t.Fatalf("systemic error measurements=%d pending=%v", measurements, runtime.groups["warp"].pendingSlow)
	}
}

func TestBandwidthFailuresDoNotSwitchWhenEveryDownloadFails(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		candidatesFn: func(context.Context, string) ([]string, error) {
			return []string{"a", "b"}, nil
		},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			return model.Sample{At: time.Now(), Node: node, Scope: "front", Kind: "confirm", Error: "connection reset", Attempts: 1, Failures: 1}, nil
		},
	}
	initial := []model.Sample{{At: time.Now(), Node: "a", Scope: "front", Kind: "quick", Error: "connection reset", Attempts: 1, Failures: 1}}
	runtime.considerSlow(context.Background(), group, "a", initial, nil)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("all failed downloads caused switch: %v", fixture.writes)
	}
}

func TestMinimumBandwidthCanPrepareSwitchWithoutHistory(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain", MinMbps: 50}
	results := []model.Sample{
		{At: time.Now(), Node: "a", Scope: "front", Kind: "confirm", Mbps: 40, Attempts: 1},
		{At: time.Now(), Node: "b", Scope: "front", Kind: "confirm", Mbps: 60, Attempts: 1},
	}
	index := 0
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		candidatesFn: func(context.Context, string) ([]string, error) {
			return []string{"a", "b"}, nil
		},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			result := results[index]
			index++
			if result.Node != node {
				t.Fatalf("measured %q, queued %q", node, result.Node)
			}
			return result, nil
		},
	}
	initial := []model.Sample{{At: time.Now(), Node: "a", Scope: "front", Kind: "quick", Mbps: 40, Attempts: 1}}
	runtime.considerSlow(context.Background(), group, "a", initial, nil)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" {
		t.Fatalf("cold-start minimum did not select eligible candidate: %q", fixture.proxies["warp"].Now)
	}
}

func TestHealthyMeasurementCancelsPendingSlowRecheck(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Schedule.RecheckDelay = 20 * time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	past := []model.Sample{{At: time.Now(), Node: "a", Scope: "front", Kind: "sweep", Mbps: 100}}
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		measureFn: func(context.Context, string, string, int64) (model.Sample, error) {
			t.Fatal("cancelled slow recheck performed a measurement")
			return model.Sample{}, nil
		},
	}
	runtime.considerSlow(context.Background(), group, "a", []model.Sample{{Node: "a", Scope: "front", Kind: "quick", Mbps: 80}}, past)
	runtime.considerSlow(context.Background(), group, "a", []model.Sample{{Node: "a", Scope: "front", Kind: "quick", Mbps: 90}}, past)
	runtime.wg.Wait()
}

func TestBackoffSequence(t *testing.T) {
	if got := []time.Duration{nextBackoff(0), nextBackoff(5 * time.Second), nextBackoff(15 * time.Second)}; got[0] != 5*time.Second || got[1] != 15*time.Second || got[2] != 30*time.Second {
		t.Fatalf("backoff sequence %v", got)
	}
}

func TestPeriodicGeoRouteProofFailureIsNotAProxyFailure(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Schedule.GeoInterval = time.Minute
	runner.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) {
		return probe.GeoResult{}, probe.ErrRoute
	}
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{"warp": {current: "a"}}}
	runtime.maybeGeoCheck(context.Background(), config.Group{Name: "warp", Chain: "chain", GeoProxy: "http://127.0.0.1:1"}, "a")
	if runtime.groups["warp"].geoFailures != 0 {
		t.Fatalf("route proof error counted as %d proxy failures", runtime.groups["warp"].geoFailures)
	}
}
