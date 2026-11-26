package guard

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
	"github.com/lonelygo/mi-guard/internal/probe"
)

func newDaemonFixture(t *testing.T) (*controllerFixture, *Runner) {
	t.Helper()
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.Cfg.Speed.QuickBytes = 256
	runner.Cfg.Speed.ConfirmBytes = 1024
	runner.Cfg.Speed.SweepBytes = 4096
	return fixture, runner
}

func daemonSample(r *Runner, node, kind string, mbps float64) model.Sample {
	size := r.Cfg.Speed.SweepBytes
	switch kind {
	case "quick":
		size = r.Cfg.Speed.QuickBytes
	case "confirm":
		size = r.Cfg.Speed.ConfirmBytes
	}
	return model.Sample{
		At: time.Now(), Node: node, Provider: "airport", Scope: "front", Kind: kind,
		Profile: r.Cfg.DownloadProfile(1, size), Mbps: mbps, Attempts: 1,
	}
}

func failedDaemonSample(r *Runner, node, kind, message string) model.Sample {
	s := daemonSample(r, node, kind, 0)
	s.Error, s.Failures = message, 1
	return s
}

func TestHeartbeatHardFailureRequiresTwoChecksAndBypassesPerformanceCooldown(t *testing.T) {
	fixture, runner := newDaemonFixture(t)
	fixture.failChainOnNode = "a"
	runner.Cfg.Groups = []config.Group{{Name: "warp", Chain: "chain", Direct: "direct"}}
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{"warp": {current: "a", cooldownUntil: time.Now().Add(time.Hour)}}}

	if err := runner.Store.Record(context.Background(), daemonSample(runner, "c", "sweep", 80)); err != nil {
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
	if fixture.proxies["warp"].Now != "c" || fixture.proxies["direct"].Now != "c" {
		t.Fatalf("recovery did not switch both selectors: warp=%q direct=%q", fixture.proxies["warp"].Now, fixture.proxies["direct"].Now)
	}
}

func TestStandaloneHeartbeatProbesSelectorAndRecoversWithoutChainState(t *testing.T) {
	fixture, runner := newDaemonFixture(t)
	fixture.failOnSelection["warp"] = "a"
	group := config.Group{Name: "warp", Mode: config.GroupModeSelector}
	runner.Cfg.Groups = []config.Group{group}
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{"warp": {current: "a"}}}

	manual := daemonSample(runner, "c", "bench", 2000)
	manual.Profile.Streams = 4
	if err := runner.Store.Record(context.Background(), manual); err != nil {
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
	if !slices.Equal(delayNames, []string{"warp", "warp", "b", "warp"}) {
		t.Fatalf("standalone fallback probes = %v", delayNames)
	}

	snapshot, err := runner.Store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Heartbeats) != 1 || snapshot.Heartbeats[0].Scope != "warp" || snapshot.Heartbeats[0].Attempts != 2 {
		t.Fatalf("standalone heartbeat aggregate = %+v", snapshot.Heartbeats)
	}
}

func TestDaemonCandidateHistoryMatchesLiveProviderRouteAndTarget(t *testing.T) {
	now := time.Unix(20_000, 0)
	nodes := []string{"a", "b", "c"}
	live := map[string]model.Proxy{
		"a": {Name: "a", Type: "Trojan", Provider: "airport"},
		"b": {Name: "b", Type: "Trojan", Provider: "other", Dialer: "warp"},
		"c": {Name: "c", Type: "Trojan", Provider: "airport"},
	}
	profile := model.DownloadProfile{Streams: 1, TotalBytes: 1024, Target: strings.Repeat("a", 64)}
	a := model.Sample{At: now.Add(-time.Hour), Node: "a", Kind: "sweep", Scope: "front", Provider: "airport", Profile: profile, Mbps: 90}
	a.Profile.TotalBytes = 4096
	b := model.Sample{At: now.Add(-3 * time.Hour), Node: "b", Kind: "quick", Scope: "chain:b", Provider: "other", Profile: profile, Mbps: 80}
	b.Profile.TotalBytes = 256
	olderA := a
	olderA.At, olderA.Mbps = now.Add(-2*time.Hour), 100
	past := []model.Sample{a, b, olderA}

	// Every newer speed is unsuitable, even with the same node name.
	high := a
	high.At, high.Mbps = now, 2000
	bench, multi, legacy := high, high, high
	bench.Kind = "bench"
	multi.Profile.Streams = 4
	legacy.Profile = model.DownloadProfile{}
	target, former := high, high
	target.Profile.Target = strings.Repeat("b", 64)
	former.Provider = "former"
	chain, front := high, high
	chain.Scope = "chain:a"
	front.Node, front.Provider = "b", "other"
	past = append(past, bench, multi, legacy, target, former, chain, front)
	bench.Node, bench.Profile.Streams = "c", 4
	past = append(past, bench)

	known, unmeasured := daemonCandidateSamples(nodes, live, past, profile, time.Time{})
	if !slices.Equal(known, []model.Sample{a, b}) || !slices.Equal(unmeasured, []model.Sample{{Node: "c"}}) {
		t.Fatalf("recovery history = %+v, fallback = %+v", known, unmeasured)
	}
	known, unmeasured = daemonCandidateSamples(nodes, live, past, profile, a.At)
	if !slices.Equal(known, []model.Sample{a}) || !slices.Equal(unmeasured, []model.Sample{{Node: "b"}, {Node: "c"}}) {
		t.Fatalf("recent cache = %+v, fallback = %+v", known, unmeasured)
	}
	known, unmeasured = daemonCandidateSamples(nodes, nil, past, profile, time.Time{})
	if len(known) != 0 || !slices.Equal(unmeasured, []model.Sample{{Node: "a"}, {Node: "b"}, {Node: "c"}}) {
		t.Fatalf("unresolved live identities = %+v, fallback = %+v", known, unmeasured)
	}
}

func TestHardRecoveryHandsOffUnmeasuredCandidatesWhenInventoryIsUnavailable(t *testing.T) {
	ctx := context.Background()
	runner := &Runner{
		Cfg:   config.Config{Speed: config.Speed{SweepBytes: 4096, ConfirmBytes: 1024}},
		Core:  mihomo.New("unsupported://controller", ""),
		Store: history.New(filepath.Join(t.TempDir(), "history.json"), 5, 1),
	}
	if err := runner.Store.Record(ctx, daemonSample(runner, "c", "sweep", 2000)); err != nil {
		t.Fatal(err)
	}
	var forValidation []model.Sample
	d := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"group": {current: "a"}},
		candidatesFn: func(context.Context, string) ([]string, error) {
			return []string{"a", "b", "c"}, nil
		},
		applyFn: func(_ context.Context, _ config.Group, _ string, ranked []model.Sample) (string, error) {
			forValidation = ranked
			return "b", nil
		},
	}
	if err := d.recoverHard(ctx, config.Group{Name: "group"}, "a"); err != nil {
		t.Fatal(err)
	}
	want := []model.Sample{{Node: "b", Kind: "recovery"}, {Node: "c", Kind: "recovery"}}
	if !slices.Equal(forValidation, want) {
		t.Fatalf("candidates for live validation = %+v, want %+v", forValidation, want)
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
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.NearBestPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	past := []model.Sample{
		daemonSample(runner, "a", "sweep", 100),
		daemonSample(runner, "a", "sweep", 100),
		daemonSample(runner, "a", "sweep", 100),
		daemonSample(runner, "b", "sweep", 100),
	}
	initial := []model.Sample{daemonSample(runner, "a", "quick", 80)}

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

	failedConfirmation := newRuntime([]model.Sample{daemonSample(runner, "a", "confirm", 81)})
	failedConfirmation.considerSlow(context.Background(), group, "a", initial, past, policy.LatencyGate{})
	failedConfirmation.wg.Wait()
	fixture.mu.Lock()
	if len(fixture.writes) != 0 {
		fixture.mu.Unlock()
		t.Fatalf("recovered confirmation switched: %v", fixture.writes)
	}
	fixture.mu.Unlock()

	changedProvider := daemonSample(runner, "a", "confirm", 70)
	changedProvider.Provider = "replacement"
	previous := daemonSample(runner, "a", "sweep", 100)
	previous.Provider = changedProvider.Provider
	replaced := newRuntime([]model.Sample{changedProvider})
	replaced.considerSlow(context.Background(), group, "a", initial, append(past, previous), policy.LatencyGate{})
	replaced.wg.Wait()

	confirmed := newRuntime([]model.Sample{
		daemonSample(runner, "a", "confirm", 79),
		daemonSample(runner, "b", "confirm", 100),
	})
	confirmed.considerSlow(context.Background(), group, "a", initial, past, policy.LatencyGate{})
	confirmed.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" {
		t.Fatalf("confirmed boundary slowdown did not select verified alternative: %q", fixture.proxies["warp"].Now)
	}
}

func TestSlowSchedulerRemeasuresHistoricalAlternative(t *testing.T) {
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.NearBestPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	past := []model.Sample{
		daemonSample(runner, "a", "sweep", 100),
		daemonSample(runner, "b", "sweep", 120),
	}
	results := []model.Sample{
		daemonSample(runner, "a", "confirm", 79),
		daemonSample(runner, "b", "confirm", 50),
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
	runtime.considerSlow(context.Background(), group, "a", []model.Sample{daemonSample(runner, "a", "quick", 80)}, past, policy.LatencyGate{})
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
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	results := []model.Sample{
		failedDaemonSample(runner, "a", "confirm", "connection reset"),
		daemonSample(runner, "b", "confirm", 90),
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
	initial := []model.Sample{failedDaemonSample(runner, "a", "quick", "zero payload")}
	runtime.considerSlow(context.Background(), group, "a", initial, nil, policy.LatencyGate{})
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" {
		t.Fatalf("two attributed failures did not recover to verified candidate: %q", fixture.proxies["warp"].Now)
	}
}

func TestAttributedDownloadFailureStopsWhenConfirmationRecovers(t *testing.T) {
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain"}
	measurements := 0
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
		measureFn: func(_ context.Context, node, _ string, _ int64) (model.Sample, error) {
			measurements++
			return daemonSample(runner, node, "confirm", 80), nil
		},
	}
	initial := []model.Sample{failedDaemonSample(runner, "a", "quick", "connection reset")}
	runtime.considerSlow(context.Background(), group, "a", initial, nil, policy.LatencyGate{})
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
	fixture, runner := newDaemonFixture(t)
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
			return failedDaemonSample(runner, node, "confirm", "connection reset"), nil
		},
	}
	initial := []model.Sample{failedDaemonSample(runner, "a", "quick", "connection reset")}
	runtime.considerSlow(context.Background(), group, "a", initial, nil, policy.LatencyGate{})
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("all failed downloads caused switch: %v", fixture.writes)
	}
}

func TestMinimumBandwidthCanPrepareSwitchWithoutHistory(t *testing.T) {
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond
	group := config.Group{Name: "warp", Chain: "chain", MinMbps: 50}
	results := []model.Sample{
		daemonSample(runner, "a", "confirm", 40),
		daemonSample(runner, "b", "confirm", 60),
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
	initial := []model.Sample{daemonSample(runner, "a", "quick", 40)}
	runtime.considerSlow(context.Background(), group, "a", initial, nil, policy.LatencyGate{})
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" {
		t.Fatalf("cold-start minimum did not select eligible candidate: %q", fixture.proxies["warp"].Now)
	}
}

func TestHealthyMeasurementCancelsPendingSlowRecheck(t *testing.T) {
	runner := &Runner{Cfg: config.Config{Speed: config.Speed{QuickBytes: 256, SweepBytes: 4096}}}
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Schedule.RecheckDelay = time.Hour
	group := config.Group{Name: "warp", Chain: "chain"}
	manual := daemonSample(runner, "a", "bench", 1000)
	manual.Profile.Streams = 4
	legacy := daemonSample(runner, "a", "sweep", 2000)
	legacy.Profile = model.DownloadProfile{}
	past := []model.Sample{manual, legacy, daemonSample(runner, "a", "sweep", 100)}
	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{"warp": {current: "a"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		runtime.wg.Wait()
	}()
	runtime.considerSlow(ctx, group, "a", []model.Sample{daemonSample(runner, "a", "quick", 80)}, past, policy.LatencyGate{})
	if !runtime.pendingMatches("warp", "a", 1) {
		t.Fatal("quick result at the daemon drop boundary did not schedule confirmation")
	}
	runtime.considerSlow(ctx, group, "a", []model.Sample{daemonSample(runner, "a", "quick", 90)}, past, policy.LatencyGate{})
	if runtime.pendingMatches("warp", "a", 1) {
		t.Fatal("healthy quick result did not cancel the pending confirmation")
	}
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
