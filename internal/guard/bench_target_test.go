package guard

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
)

func TestResolveBenchTargetMatchesGroupAndChainIdentities(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	const front = "🇹🇼 Taiwan Σ"
	const chain = "👩‍💻 Taiwan [Exit]"
	managed := config.Group{Name: front, Chain: chain, Direct: "direct", GeoProxy: "http://127.0.0.1:1", ExpectedGeo: "TW", StrictGeo: true, Exclude: "^c$", MinMbps: 45}
	r.Cfg.Groups = []config.Group{managed}
	f.proxies[front] = model.Proxy{Name: front, Type: "Selector", Now: "a", All: []string{"a", "b"}}
	f.proxies[chain] = model.Proxy{Name: chain, Type: "WireGuard", Dialer: front}
	f.proxies["Taiwan"] = model.Proxy{Name: "Taiwan", Type: "Fallback", Now: "b", All: []string{"b"}}
	f.proxies["Taiwan leaf"] = model.Proxy{Name: "Taiwan leaf", Type: "Trojan"}
	f.proxies["No-group dialer"] = model.Proxy{Name: "No-group dialer", Type: "Trojan", Dialer: "b"}
	ctx := context.Background()
	for _, query := range []string{front, chain, "ς", "[exit]", "👩‍💻"} {
		got, err := r.ResolveBenchTarget(ctx, query)
		if err != nil || got.Group != managed || got.Type != "Selector" || got.Current != "a" {
			t.Errorf("ResolveBenchTarget(%q) = %+v, %v", query, got, err)
		}
	}
	got, err := r.ResolveBenchTarget(ctx, "Taiwan")
	if err != nil || got.Group.Name != "Taiwan" || got.Type != "Fallback" || got.Current != "b" {
		t.Fatalf("literal before substring = %+v, %v", got, err)
	}
	want := strings.Join([]string{model.QuoteName("Taiwan"), model.QuoteName(front), model.QuoteName(chain)}, ", ")
	if _, err := r.ResolveBenchTarget(ctx, "taiWan"); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("ambiguous matches = %v, want sorted full names %s", err, want)
	}
	for _, query := range []string{"Taiwan leaf", "No-group dialer", "   "} {
		if _, err := r.ResolveBenchTarget(ctx, query); err == nil {
			t.Errorf("unexpected target for %q", query)
		}
	}
	got, err = r.ResolveBenchTarget(ctx, "chain")
	if err != nil || got.Group.Name != "warp" || got.Group.Chain != "chain" || got.Group.Mode != config.GroupModeChain || got.Current != "a" {
		t.Errorf("unconfigured chain = %+v, %v", got, err)
	}
	f.mu.Lock()
	f.proxies[chain] = model.Proxy{Name: chain, Type: "WireGuard", Dialer: "direct"}
	f.mu.Unlock()
	if _, err := r.ResolveBenchTarget(ctx, chain); err == nil {
		t.Fatal("a reconfigured chain redirected a managed target to another live group")
	}
}

func TestGroupQueryAcceptsAnEquivalentDecoratedName(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	const group = "🇺🇸 美国节点"
	f.proxies[group] = model.Proxy{Name: group, Type: "Selector", Now: "a", All: []string{"a", "b"}}
	for _, q := range []string{group, "🇺🇲 美国节点", "美国节点", "美国"} {
		tgt, err := r.ResolveBenchTarget(context.Background(), q)
		if err != nil || tgt.Group.Name != group {
			t.Fatalf("query %q returned %+v, %v", q, tgt, err)
		}
	}
}

func TestGlobalCandidatesProtectDedicatedMeasurementSelector(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	ctx := context.Background()
	global := model.Proxy{Name: "GLOBAL", Type: "Selector", Now: "a", All: []string{"bench", "a"}}
	f.mu.Lock()
	f.proxies["GLOBAL"] = global
	f.mu.Unlock()
	got, err := r.Candidates(ctx, "GLOBAL")
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("GLOBAL candidates = %v, %v", got, err)
	}
	tgt, err := r.ResolveBenchTarget(ctx, "bench")
	if err != nil || tgt.Group.Name != "bench" {
		t.Fatalf("dedicated scan target = %+v, %v", tgt, err)
	}
	got, err = r.Candidates(ctx, tgt.Group.Name)
	if err != nil || !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("dedicated scan candidates = %v, %v", got, err)
	}
	global.Now = "bench"
	f.mu.Lock()
	f.proxies["GLOBAL"] = global
	f.mu.Unlock()
	if _, err := r.Candidates(ctx, "bench"); err == nil {
		t.Fatal("allowed a scan through the active GLOBAL selection")
	}
}

func TestBenchCandidatesLimitOnlyManualApplyRankingsToDirectMembers(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	group := model.Proxy{Name: "ordinary", Type: "Selector", Now: "a", All: []string{"nested", "a"}}
	f.proxies[group.Name] = group
	f.proxies["nested"] = model.Proxy{Name: "nested", Type: "Fallback", All: []string{"c", "b"}}
	ctx := context.Background()
	target, err := r.ResolveBenchTarget(ctx, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := r.BenchCandidates(ctx, target, false)
	if err != nil || !slices.Equal(scan, []string{"c", "b", "a"}) {
		t.Fatalf("scan candidates = %v, %v", scan, err)
	}
	apply, err := r.BenchCandidates(ctx, target, true)
	if err != nil || !slices.Equal(apply, []string{"a"}) {
		t.Fatalf("apply candidates = %v, %v", apply, err)
	}
	f.mu.Lock()
	f.proxies["outer"] = model.Proxy{Name: "outer", Type: "Selector", Now: "nested", All: []string{"nested"}}
	f.mu.Unlock()
	outer, err := r.ResolveBenchTarget(ctx, "outer")
	if err != nil {
		t.Fatal(err)
	}
	leaves, err := r.BenchCandidates(ctx, outer, true)
	if err != nil || !slices.Equal(leaves, []string{"c", "b"}) {
		t.Fatalf("nested-only scan = %v, %v", leaves, err)
	}
	actions, err := r.ApplyBenchTarget(ctx, outer, []model.Sample{{Node: "c", Mbps: 25}})
	if err != nil || len(actions) != 1 || actions[0].Action != "skipped" || len(f.writes) != 0 {
		t.Fatalf("nested-only selection = %+v, %v", actions, err)
	}

	r.Cfg.Groups = []config.Group{{Name: group.Name, Mode: config.GroupModeSelector, Exclude: "^b$"}}
	group.Type = "URLTest"
	f.mu.Lock()
	f.proxies[group.Name] = group
	f.mu.Unlock()
	target, err = r.ResolveBenchTarget(ctx, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.BenchCandidates(ctx, target, true)
	if err != nil || !slices.Equal(got, []string{"c", "a"}) {
		t.Fatalf("automatic group scan with --apply = %v, %v", got, err)
	}
	out, err := r.ApplyBenchTarget(ctx, target, nil)
	if err != nil || len(out) != 1 || out[0].Action != "skipped" || !strings.Contains(out[0].Reason, "不支持手动切换") {
		t.Fatalf("automatic group selection = %+v, %v", out, err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("automatic group writes = %v", f.writes)
	}
}
