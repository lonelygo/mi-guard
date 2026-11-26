package guard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
)

func latencyDaemonSample(r *Runner, node, kind string, mbps float64, latencyMS int) model.Sample {
	sample := daemonSample(r, node, kind, mbps)
	sample.Attempts = 2
	sample.LatencyMS = latencyMS
	return sample
}

func newLatencyDaemonRuntime(t *testing.T, group config.Group) (*controllerFixture, *Runner, *daemonRuntime, *string) {
	t.Helper()
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.SevereDropPct = 20
	runner.Cfg.Selection.NearBestPct = 20
	runner.Cfg.Selection.SlowConfirmations = 2
	runner.Cfg.Selection.MaxAge = 24 * time.Hour
	runner.Cfg.Schedule.RecheckDelay = time.Millisecond

	runtime := &daemonRuntime{
		runner: runner,
		groups: map[string]*groupRuntime{group.Name: {current: "a"}},
		candidatesFn: func(context.Context, string) ([]string, error) {
			return []string{"a", "b", "c"}, nil
		},
		measureFn: func(_ context.Context, node, kind string, _ int64) (model.Sample, error) {
			t.Fatalf("unexpected measurement of %s (%s)", node, kind)
			return model.Sample{}, nil
		},
	}
	var notification string
	runner.NotifyEvent = func(_, _, body, _ string) { notification = body }
	return fixture, runner, runtime, &notification
}

func latencyBaseline(runner *Runner) []model.Sample {
	return []model.Sample{
		latencyDaemonSample(runner, "a", "sweep", 100, 100),
		latencyDaemonSample(runner, "a", "sweep", 100, 100),
		latencyDaemonSample(runner, "a", "sweep", 100, 100),
	}
}

func TestHealthyBandwidthLatencyOutlierDoesNotStartConfirmation(t *testing.T) {
	group := config.Group{Name: "warp", Chain: "chain"}
	fixture, runner, runtime, notification := newLatencyDaemonRuntime(t, group)
	gate := policy.LatencyGate{MeanMS: 100, Samples: 20, Factor: 1.5}
	fresh := []model.Sample{
		latencyDaemonSample(runner, "a", "sweep", 100, 180),
		latencyDaemonSample(runner, "b", "sweep", 110, 100),
	}

	runtime.considerSlow(context.Background(), group, "a", fresh, latencyBaseline(runner), gate)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if runtime.groups[group.Name].pendingSlow || len(fixture.writes) != 0 || fixture.proxies["warp"].Now != "a" || *notification != "" {
		t.Fatalf("healthy bandwidth pending=%v writes=%v selected=%q notification=%q",
			runtime.groups[group.Name].pendingSlow, fixture.writes, fixture.proxies["warp"].Now, *notification)
	}
}

func TestBandwidthDropFiltersFastLatencyOutlierAndSelectsFasterQualifiedNode(t *testing.T) {
	group := config.Group{Name: "warp", Chain: "chain"}
	fixture, runner, runtime, notification := newLatencyDaemonRuntime(t, group)
	runtime.measureFn = func(_ context.Context, node, kind string, _ int64) (model.Sample, error) {
		if node != "a" || kind != "confirm" {
			t.Fatalf("unexpected measurement of %s (%s)", node, kind)
		}
		return latencyDaemonSample(runner, "a", "confirm", 69, 100), nil
	}
	gate := policy.LatencyGate{MeanMS: 100, Samples: 20, Factor: 1.5}
	fresh := []model.Sample{
		latencyDaemonSample(runner, "a", "sweep", 70, 100),
		latencyDaemonSample(runner, "b", "sweep", 110, 180),
		latencyDaemonSample(runner, "c", "sweep", 100, 140),
	}

	runtime.considerSlow(context.Background(), group, "a", fresh, latencyBaseline(runner), gate)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "c" || strings.Join(fixture.writes, ",") != "warp=c" {
		t.Fatalf("latency-filtered slowdown selected=%q writes=%v", fixture.proxies["warp"].Now, fixture.writes)
	}
	if !strings.Contains(*notification, "持续降速确认") || strings.Contains(*notification, "持续延迟超限") {
		t.Fatalf("slowdown notification = %q", *notification)
	}
}

func TestBandwidthDropKeepsWhenQualifiedAlternativesAreSlower(t *testing.T) {
	group := config.Group{Name: "warp", Chain: "chain"}
	fixture, runner, runtime, notification := newLatencyDaemonRuntime(t, group)
	runtime.measureFn = func(_ context.Context, node, kind string, _ int64) (model.Sample, error) {
		return latencyDaemonSample(runner, node, kind, 69, 100), nil
	}
	gate := policy.LatencyGate{MeanMS: 100, Samples: 20, Factor: 1.5}
	fresh := []model.Sample{
		latencyDaemonSample(runner, "a", "sweep", 70, 100),
		latencyDaemonSample(runner, "b", "sweep", 110, 180),
		latencyDaemonSample(runner, "c", "sweep", 60, 100),
	}

	runtime.considerSlow(context.Background(), group, "a", fresh, latencyBaseline(runner), gate)
	runtime.wg.Wait()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 || fixture.proxies["warp"].Now != "a" || *notification != "" {
		t.Fatalf("slower qualified alternative writes=%v selected=%q notification=%q",
			fixture.writes, fixture.proxies["warp"].Now, *notification)
	}
}

func TestLatencySelectionKeepsWhenEveryCandidateIsExcludedOrCooldownIsActive(t *testing.T) {
	gate := policy.LatencyGate{MeanMS: 100, Samples: 20, Factor: 1.5}
	group := config.Group{Name: "warp", Chain: "chain", Direct: "direct"}

	t.Run("all excluded", func(t *testing.T) {
		fixture, runner, runtime, notification := newLatencyDaemonRuntime(t, group)
		runtime.measureFn = func(_ context.Context, node, kind string, _ int64) (model.Sample, error) {
			return latencyDaemonSample(runner, node, kind, 69, 190), nil
		}
		fresh := []model.Sample{
			latencyDaemonSample(runner, "a", "sweep", 70, 180),
			latencyDaemonSample(runner, "b", "sweep", 110, 170),
			latencyDaemonSample(runner, "c", "sweep", 100, 160),
		}
		runtime.considerSlow(context.Background(), group, "a", fresh, latencyBaseline(runner), gate)
		runtime.wg.Wait()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if len(fixture.writes) != 0 || fixture.proxies["warp"].Now != "a" || fixture.proxies["direct"].Now != "a" || *notification != "" {
			t.Fatalf("all-excluded writes=%v primary=%q follower=%q notification=%q",
				fixture.writes, fixture.proxies["warp"].Now, fixture.proxies["direct"].Now, *notification)
		}
	})

	t.Run("cooldown", func(t *testing.T) {
		fixture, runner, runtime, notification := newLatencyDaemonRuntime(t, group)
		runtime.groups[group.Name].cooldownUntil = time.Now().Add(time.Hour)
		runtime.measureFn = func(_ context.Context, node, kind string, _ int64) (model.Sample, error) {
			return latencyDaemonSample(runner, node, kind, 69, 100), nil
		}
		fresh := []model.Sample{
			latencyDaemonSample(runner, "a", "sweep", 70, 100),
			latencyDaemonSample(runner, "b", "sweep", 100, 100),
		}
		runtime.considerSlow(context.Background(), group, "a", fresh, latencyBaseline(runner), gate)
		runtime.wg.Wait()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if len(fixture.writes) != 0 || fixture.proxies["warp"].Now != "a" || fixture.proxies["direct"].Now != "a" || *notification != "" {
			t.Fatalf("cooldown writes=%v primary=%q follower=%q notification=%q",
				fixture.writes, fixture.proxies["warp"].Now, fixture.proxies["direct"].Now, *notification)
		}
	})
}

func TestHardRecoveryIgnoresLatencyBaseline(t *testing.T) {
	fixture, runner := newDaemonFixture(t)
	runner.Cfg.Selection.LatencyOutlierFactor = 1.5
	group := config.Group{Name: "warp", Chain: "chain"}
	runtime := &daemonRuntime{runner: runner, groups: map[string]*groupRuntime{"warp": {current: "a"}}}

	baseline := latencyDaemonSample(runner, "baseline", "sweep", 50, 50)
	if err := runner.RecordGroupLatencies(context.Background(), group.Name, []model.Sample{baseline}); err != nil {
		t.Fatal(err)
	}
	candidate := latencyDaemonSample(runner, "b", "sweep", 100, 1000)
	if err := runner.Store.Record(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	gate, err := runner.GroupLatencyGate(group.Name)
	if err != nil || gate.Exclusion(candidate) != policy.LatencyAboveLimit {
		t.Fatalf("test did not create a rejecting latency gate: gate=%+v err=%v", gate, err)
	}
	var notification string
	runner.NotifyEvent = func(_, _, body, _ string) { notification = body }
	if err := runtime.recoverHard(context.Background(), group, "a"); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "b" || !strings.Contains(notification, "链路故障恢复") || strings.Contains(notification, "持续降速确认") {
		t.Fatalf("hard recovery selected=%q notification=%q writes=%v", fixture.proxies["warp"].Now, notification, fixture.writes)
	}
}
