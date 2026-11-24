package guard

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

func TestApplyBenchTargetDirectMembersOnlyAndConfirmedHistory(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	f.proxies["ordinary"] = model.Proxy{Name: "ordinary", Type: "Selector", Now: "a", All: []string{"a", "nested", "b", "c"}}
	f.proxies["nested"] = model.Proxy{Name: "nested", Type: "Selector", Now: "child", All: []string{"child"}}
	f.proxies["child"] = model.Proxy{Name: "child", Type: "Trojan"}
	ctx := context.Background()
	if _, err := r.Store.RecordSelection(ctx, "ordinary", "a", time.Now().Add(-48*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	target, err := r.ResolveBenchTarget(ctx, "ordin")
	if err != nil {
		t.Fatal(err)
	}
	ranked := []model.Sample{
		{Node: "child", Mbps: 999}, {Node: "nested", Mbps: 900}, {Node: "b", Mbps: 800, Error: "failed"},
		{Node: "b", Mbps: math.Inf(1)}, {Node: "b", Mbps: math.NaN()}, {Node: "b", Kind: "recovery"},
		{Node: "c", Mbps: 25}, {Node: "a", Mbps: 20},
	}
	out, err := r.ApplyBenchTarget(ctx, target, ranked)
	want := SelectionResult{Group: "ordinary", Before: "a", After: "c", Action: "switched"}
	if err != nil || len(out) != 1 || out[0] != want || !slices.Equal(f.writes, []string{"ordinary=c"}) {
		t.Fatalf("manual apply = %+v, %v, writes %v", out, err, f.writes)
	}
	if _, err = r.ApplyBenchTarget(ctx, target, ranked); !errors.Is(err, ErrManualOverride) {
		t.Fatalf("stale target error = %v", err)
	}
	snapshot, err := r.Store.Snapshot(ctx)
	if got := snapshot.Selections["ordinary"]; err != nil || got.Node != "c" || !got.GuardSelected || got.Longest["a"] < 48*time.Hour {
		t.Fatalf("confirmed history = %+v, %v", snapshot.Selections, err)
	}

	target.Current = "c"
	r.Cfg.Selection.FollowGroups = map[string]string{"ordinary": "direct"}
	f.mu.Lock()
	f.proxyReads = nil
	f.mu.Unlock()
	out, err = r.ApplyBenchTarget(ctx, target, ranked)
	if err != nil || len(out) != 1 || out[0].Action != "kept" || out[0].After != "c" || slices.Contains(f.proxyReads, "direct") || len(f.writes) != 1 {
		t.Fatalf("retain = %+v, %v, reads %v, writes %v", out, err, f.proxyReads, f.writes)
	}
	later, err := r.Store.Snapshot(ctx)
	earlier := snapshot.Selections["ordinary"]
	if got := later.Selections["ordinary"]; err != nil || got.Since != earlier.Since || !got.LastSeen.After(earlier.LastSeen) || len(later.Selections) != 1 {
		t.Fatalf("retained history = %+v after %+v, %v", later.Selections, earlier, err)
	}
}

func TestApplyBenchTargetFollowerOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, typ, initial, external, action, after, reason string
		members                                             []string
	}{
		{"switch", "Selector", "a", "", "switched", "b", "", []string{"a", "b"}},
		{"already selected", "Selector", "b", "", "kept", "b", "同一节点", []string{"a", "b"}},
		{"automatic", "URLTest", "a", "", "skipped", "a", "不支持手动切换", []string{"a", "b"}},
		{"absent member", "Selector", "a", "", "skipped", "a", "同名", []string{"a"}},
		{"concurrent edit", "Selector", "a", "c", "skipped", "c", "其他操作", []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, server := newControllerFixture(t)
			r := testRunner(t, f, server)
			const follower = "普通组"
			f.proxies[follower] = model.Proxy{Name: follower, Type: tc.typ, Now: tc.initial, All: tc.members}
			r.Cfg.Groups = []config.Group{{Name: "warp", Chain: "chain", Direct: "direct"}}
			r.Cfg.Selection.FollowGroups = map[string]string{"warp": follower}
			f.afterSelect = func(group, _ string) {
				if group == "warp" && tc.external != "" {
					p := f.proxies[follower]
					p.Now = tc.external
					f.proxies[follower] = p
				}
			}
			ctx := context.Background()
			target, err := r.ResolveBenchTarget(ctx, "chain")
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.ApplyBenchTarget(ctx, target, []model.Sample{{Node: "b", Mbps: 60}})
			if err != nil || len(out) != 2 || out[0] != (SelectionResult{Group: "warp", Before: "a", After: "b", Action: "switched"}) {
				t.Fatalf("primary = %+v, %v", out, err)
			}
			if got := out[1]; got.Group != follower || got.Before != tc.initial || got.After != tc.after || got.Action != tc.action || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("follower = %+v", got)
			}
			writes := []string{"warp=b"}
			if tc.action == "switched" {
				writes = append(writes, follower+"=b")
			}
			if !slices.Equal(f.writes, writes) {
				t.Fatalf("writes %v, want %v", f.writes, writes)
			}
			state, err := r.Store.Snapshot(ctx)
			if err != nil || state.Selections["warp"].Node != "b" || !state.Selections["warp"].GuardSelected {
				t.Fatalf("primary history = %+v, %v", state.Selections, err)
			}
			obs, ok := state.Selections[follower]
			if ok != (tc.action != "skipped") || ok && (obs.Node != "b" || obs.GuardSelected != (tc.action == "switched")) {
				t.Fatalf("follower history = %+v", state.Selections)
			}
		})
	}
}

func TestApplyBenchTargetIsolatesDedicatedSelector(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	r.Store = nil
	f.proxies["GLOBAL"] = model.Proxy{Name: "GLOBAL", Type: "Selector", Now: "a", All: []string{"a", "b", "bench"}}
	ctx := context.Background()
	target, err := r.ResolveBenchTarget(ctx, "bench")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.ApplyBenchTarget(ctx, target, []model.Sample{{Node: "b", Mbps: 10}})
	if err != nil || len(out) != 1 || out[0].Action != "skipped" || !strings.Contains(out[0].Reason, "测速专用组") {
		t.Fatalf("dedicated target = %+v, %v", out, err)
	}
	target, err = r.ResolveBenchTarget(ctx, "GLOBAL")
	if err != nil {
		t.Fatal(err)
	}
	out, err = r.ApplyBenchTarget(ctx, target, []model.Sample{{Node: "bench", Mbps: 300}, {Node: "c", Mbps: 200}})
	if err != nil || len(out) != 1 || out[0].Action != "skipped" || len(f.writes) != 0 {
		t.Fatalf("GLOBAL = %+v, %v, writes %v", out, err, f.writes)
	}
	r.Cfg.Selection.FollowGroups = map[string]string{"GLOBAL": "bench"}
	out, err = r.ApplyBenchTarget(ctx, target, []model.Sample{{Node: "b", Mbps: 10}})
	if err != nil || len(out) != 2 || out[1].Action != "skipped" || !slices.Equal(f.writes, []string{"GLOBAL=b"}) {
		t.Fatalf("dedicated follower = %+v, %v, writes %v", out, err, f.writes)
	}
}

func TestApplyBenchTargetReturnsConfirmedPrimaryOnPartialError(t *testing.T) {
	for _, failing := range []string{"follower route", "follower missing", "history"} {
		t.Run(failing, func(t *testing.T) {
			f, server := newControllerFixture(t)
			r := testRunner(t, f, server)
			r.Cfg.Selection.FollowGroups = map[string]string{"warp": "direct"}
			switch failing {
			case "follower route":
				f.failing["direct"] = true
			case "follower missing":
				delete(f.proxies, "direct")
			case "history":
				path := filepath.Join(t.TempDir(), "broken.json")
				if err := os.WriteFile(path+".selections.json", []byte("invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
				r.Store = history.New(path, 1, 1)
			}
			ctx := context.Background()
			target, err := r.ResolveBenchTarget(ctx, "chain")
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.ApplyBenchTarget(ctx, target, []model.Sample{{Node: "b", Mbps: 30}})
			if err == nil || len(out) != 2 || out[0].Group != "warp" || out[0].Action != "switched" || f.proxies["warp"].Now != "b" {
				t.Fatalf("partial apply = %+v, %v, writes %v", out, err, f.writes)
			}
			if failing == "history" {
				if !strings.Contains(err.Error(), "history") || out[1].Action != "switched" || f.proxies["direct"].Now != "b" {
					t.Fatalf("history failure = %+v, %v, writes %v", out, err, f.writes)
				}
			} else if out[1].Action != "skipped" || out[1].Reason == "" || failing == "follower route" && f.proxies["direct"].Now != "a" {
				t.Fatalf("failed follower = %+v, writes %v", out[1], f.writes)
			}
		})
	}
}

func TestGeoFallbackRestoresOriginalBeforeRecordingKeep(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	r.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) { return probe.GeoResult{}, nil }
	g := config.Group{Name: "warp", Chain: "chain", Direct: "direct", GeoProxy: "fixture"}
	ctx := context.Background()
	out, err := r.ApplyBenchTarget(ctx, BenchTarget{Group: g, Current: "a", Type: "Selector"}, []model.Sample{{Node: "a", Mbps: 50}, {Node: "b", Mbps: 40}})
	if err != nil || len(out) != 1 || out[0].Action != "kept" || out[0].After != "a" || f.proxies["warp"].Now != "a" || !slices.Equal(f.writes, []string{"warp=b", "warp=a"}) {
		t.Fatalf("fallback = %+v, %v, writes %v", out, err, f.writes)
	}
	state, err := r.Store.Snapshot(ctx)
	if err != nil || len(state.Selections) != 1 || state.Selections["warp"].Node != "a" || state.Selections["warp"].GuardSelected {
		t.Fatalf("fallback history = %+v, %v", state.Selections, err)
	}
}

func TestApplyBenchTargetHonorsManagedPolicyAtWrite(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	g := config.Group{Name: "warp", Chain: "chain", Direct: "direct", Exclude: "^c$", MinMbps: 50, StrictGeo: true, GeoProxy: "fixture", ExpectedGeo: "expected"}
	r.Cfg.Groups = []config.Group{g}
	r.geoCheck = func(_ context.Context, checked config.Group) (probe.GeoResult, error) {
		if checked != g {
			t.Fatalf("geo checked with %+v, want %+v", checked, g)
		}
		return probe.GeoResult{Domain: "unexpected"}, nil
	}
	ctx := context.Background()
	target := BenchTarget{Group: config.Group{Name: g.Name}, Type: "Selector", Current: "a"}
	out, err := r.ApplyBenchTarget(ctx, target, []model.Sample{{Node: "c", Mbps: 100}, {Node: "a", Mbps: 20}, {Node: "b", Mbps: 60}})
	if len(out) != 0 || err == nil || !strings.Contains(err.Error(), "geo") || !slices.Equal(f.writes, []string{"warp=b", "warp=a"}) {
		t.Fatalf("managed policy = %+v, %v, writes %v", out, err, f.writes)
	}
	state, err := r.Store.Snapshot(ctx)
	if err != nil || len(state.Selections) != 0 {
		t.Fatalf("failed trials entered history: %+v, %v", state.Selections, err)
	}
}

func TestDaemonApplyCanRecoverUnmeasuredCandidateAndRecordFollower(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	r.Cfg.Selection.FollowGroups = map[string]string{"warp": "direct"}
	ctx := context.Background()
	got, err := r.Apply(ctx, config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "b", Kind: "recovery"}})
	if err != nil || got != "b" || !slices.Equal(f.writes, []string{"warp=b", "direct=b"}) {
		t.Fatalf("daemon recovery = %q, %v, writes %v", got, err, f.writes)
	}
	state, err := r.Store.Snapshot(ctx)
	if err != nil || !state.Selections["warp"].GuardSelected || !state.Selections["direct"].GuardSelected {
		t.Fatalf("daemon recovery history = %+v, %v", state.Selections, err)
	}
}

func TestFollowerLeavesOperatorChangeToPrimaryAlone(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	front := f.proxies["warp"]
	front.Now = "c" // The primary changed again after its confirmed switch to b.
	f.proxies["warp"] = front
	g := config.Group{Name: "warp", Chain: "chain"}
	out, err := r.followSelection(context.Background(), "direct", f.proxies["direct"], nil, g, model.Sample{Node: "b", Mbps: 30})
	if !errors.Is(err, ErrManualOverride) || out.Action != "skipped" || out.After != "a" || len(f.writes) != 0 {
		t.Fatalf("stale primary follow = %+v, %v, writes %v", out, err, f.writes)
	}
}
