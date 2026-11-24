package guard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

type controllerFixture struct {
	mu sync.Mutex

	proxies         map[string]model.Proxy
	failing         map[string]bool
	failChainOnNode string
	failOnSelection map[string]string
	writes          []string
	delayNames      []string
	proxyReads      []string
	connectionReads int
	afterSelect     func(group, node string)
}

func newControllerFixture(t *testing.T) (*controllerFixture, *httptest.Server) {
	t.Helper()
	fixture := &controllerFixture{
		proxies: map[string]model.Proxy{
			"warp":   {Name: "warp", Type: "Selector", Now: "a", All: []string{"a", "b", "c"}},
			"direct": {Name: "direct", Type: "Selector", Now: "a", All: []string{"a", "b", "c"}},
			"bench":  {Name: "bench", Type: "Selector", Now: "a", All: []string{"a", "b", "c"}},
			"a":      {Name: "a", Type: "Trojan", Provider: "airport"},
			"b":      {Name: "b", Type: "Trojan", Provider: "airport"},
			"c":      {Name: "c", Type: "Vless", Provider: "airport"},
			"chain":  {Name: "chain", Type: "WireGuard", Dialer: "warp"},
		},
		failing:         make(map[string]bool),
		failOnSelection: make(map[string]string),
	}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	return fixture, server
}

func (f *controllerFixture) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.URL.Path == "/connections" {
		f.connectionReads++
		_ = json.NewEncoder(writer).Encode(map[string]any{"connections": []any{}})
		return
	}
	if request.URL.Path == "/proxies" {
		_ = json.NewEncoder(writer).Encode(map[string]any{"proxies": f.proxies})
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/proxies/") {
		http.NotFound(writer, request)
		return
	}
	raw := strings.TrimPrefix(request.URL.Path, "/proxies/")
	delay := strings.HasSuffix(raw, "/delay")
	if delay {
		raw = strings.TrimSuffix(raw, "/delay")
	}
	name, _ := url.PathUnescape(raw)
	if delay {
		f.delayNames = append(f.delayNames, name)
		failed := f.failing[name]
		if name == "chain" && f.failChainOnNode != "" && f.proxies["warp"].Now == f.failChainOnNode {
			failed = true
		}
		if selected, ok := f.failOnSelection[name]; ok && f.proxies["warp"].Now == selected {
			failed = true
		}
		if failed {
			http.Error(writer, "unavailable", http.StatusGatewayTimeout)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]int{"delay": 20})
		return
	}
	switch request.Method {
	case http.MethodGet:
		f.proxyReads = append(f.proxyReads, name)
		proxy, ok := f.proxies[name]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(proxy)
	case http.MethodPut:
		var body struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			http.Error(writer, "bad json", http.StatusBadRequest)
			return
		}
		proxy := f.proxies[name]
		proxy.Now = body.Name
		f.proxies[name] = proxy
		f.writes = append(f.writes, name+"="+body.Name)
		if f.afterSelect != nil {
			f.afterSelect(name, body.Name)
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		writer.WriteHeader(http.StatusNoContent)
	}
}

func TestApplyStandaloneBenchResultUsesItsLivePathWithoutChainCleanup(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	group := config.Group{Name: "warp", Mode: config.GroupModeSelector, Direct: "direct"}

	selected, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Kind: "bench", Mbps: 80}})
	if err != nil || selected != "b" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if got := strings.Join(fixture.writes, ","); got != "warp=b" {
		t.Fatalf("standalone writes %q", got)
	}
	if got := strings.Join(fixture.delayNames, ","); got != "b,warp" {
		t.Fatalf("standalone probes %q, want candidate then live selector", got)
	}
	if fixture.connectionReads != 0 {
		t.Fatalf("standalone snapshotted chain connections %d times", fixture.connectionReads)
	}
}

func TestApplyStandaloneRollsBackOnLivePathFailureWithoutConnections(t *testing.T) {
	fixture, server := newControllerFixture(t)
	fixture.failOnSelection["warp"] = "b"
	runner := testRunner(t, fixture, server)
	group := config.Group{Name: "warp", Mode: config.GroupModeSelector, StrictGeo: true}

	selected, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if selected != "" || err == nil {
		t.Fatalf("Apply = %q, %v; want failed validation", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "a" {
		t.Fatalf("standalone rollback left %q selected", fixture.proxies["warp"].Now)
	}
	if got := strings.Join(fixture.writes, ","); got != "warp=b,warp=a" {
		t.Fatalf("standalone rollback writes %q", got)
	}
	if fixture.connectionReads != 0 {
		t.Fatalf("standalone rollback snapshotted connections %d times", fixture.connectionReads)
	}
}

func TestApplyStandalonePreservesExternalTargetChange(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.afterSelect = func(group, node string) {
		if group == "warp" && node == "b" {
			proxy := fixture.proxies[group]
			proxy.Now = "c"
			fixture.proxies[group] = proxy
		}
	}

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Mode: config.GroupModeSelector}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if selected != "" || !errors.Is(err, ErrManualOverride) {
		t.Fatalf("Apply = %q, %v; want manual override", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "c" {
		t.Fatalf("external target change was overwritten with %q", fixture.proxies["warp"].Now)
	}
	if fixture.connectionReads != 0 {
		t.Fatalf("standalone external-change path read connections %d times", fixture.connectionReads)
	}
}

func TestApplyStandaloneRejectsWrongGroupAndNonPhysicalMember(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	group := config.Group{Name: "warp", Mode: config.GroupModeSelector}

	fixture.mu.Lock()
	proxy := fixture.proxies["warp"]
	proxy.Type = "URLTest"
	fixture.proxies["warp"] = proxy
	fixture.mu.Unlock()
	if _, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Mbps: 80}}); err == nil || !strings.Contains(err.Error(), "want Selector") {
		t.Fatalf("wrong group Apply error = %v", err)
	}

	fixture.mu.Lock()
	proxy = fixture.proxies["warp"]
	proxy.Type = "Selector"
	proxy.All = append(proxy.All, "nested")
	fixture.proxies["warp"] = proxy
	fixture.proxies["nested"] = model.Proxy{Name: "nested", Type: "Selector", Now: "b", All: []string{"b"}}
	fixture.mu.Unlock()
	if _, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "nested", Mbps: 80}}); err == nil || !strings.Contains(err.Error(), "not a physical proxy") {
		t.Fatalf("non-physical Apply error = %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("invalid standalone targets wrote %v", fixture.writes)
	}
}

func TestApplyStandaloneReportsCandidateReadFailure(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.mu.Lock()
	delete(fixture.proxies, "b")
	fixture.mu.Unlock()

	_, err := runner.Apply(context.Background(), config.Group{Name: "warp", Mode: config.GroupModeSelector}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if err == nil || !strings.Contains(err.Error(), "b read:") || strings.Contains(err.Error(), "not a physical proxy") {
		t.Fatalf("candidate read error = %v", err)
	}
}

func TestCandidatesExpandsUnmanagedAggregateButRequiresManagedSelector(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.mu.Lock()
	target := fixture.proxies["warp"]
	target.Type = "URLTest"
	target.All = []string{"b"}
	fixture.proxies["warp"] = target
	fixture.mu.Unlock()

	nodes, err := runner.Candidates(context.Background(), "warp")
	if err != nil {
		t.Fatalf("unmanaged URLTest candidates: %v", err)
	}
	if got := strings.Join(nodes, ","); got != "b" {
		t.Fatalf("unmanaged URLTest candidates = %q", got)
	}

	runner.Cfg.Groups = []config.Group{{Name: "warp", Mode: config.GroupModeSelector}}
	if _, err := runner.Candidates(context.Background(), "warp"); err == nil || !strings.Contains(err.Error(), "want Selector") {
		t.Fatalf("managed URLTest error = %v", err)
	}
}

func testRunner(t *testing.T, fixture *controllerFixture, server *httptest.Server) *Runner {
	t.Helper()
	runner := &Runner{
		Cfg: config.Config{
			Mihomo: config.Core{TestGroup: "bench"},
			Schedule: config.Schedule{
				HeartbeatTimeout:  100 * time.Millisecond,
				HeartbeatFailures: 2,
				Cooldown:          time.Minute,
			},
			Speed: config.Speed{LivenessURL: "https://example.test/204", Timeout: time.Second},
			Selection: config.Selection{
				SyncDirect:  true,
				NearBestPct: 20,
				MaxAge:      24 * time.Hour,
			},
			Notifications: config.Notifications{Enabled: true, Switch: true},
		},
		Core:  mihomo.New(server.URL, ""),
		Store: history.New(filepath.Join(t.TempDir(), "history.json"), 50, 1),
	}
	runner.geoCheck = func(ctx context.Context, group config.Group) (probe.GeoResult, error) {
		return probe.Geo(ctx, group.GeoProxy, runner.Cfg.Speed.GeoURL, runner.Cfg.Speed.TraceURL, group.ExpectedGeo, runner.Cfg.Speed.Timeout)
	}
	return runner
}

func TestApplySwitchesChainBeforeBestEffortDirect(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	var notification string
	var notificationEventID string
	runner.NotifyEvent = func(_, _, body, eventID string) {
		notification, notificationEventID = body, eventID
	}

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", Direct: "direct"}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if err != nil {
		t.Fatal(err)
	}
	if selected != "b" {
		t.Fatalf("selected %q, want b", selected)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if got := strings.Join(fixture.writes, ","); got != "warp=b,direct=b" {
		t.Fatalf("write order %q", got)
	}
	if !strings.Contains(notification, "直出组已跟随") {
		t.Fatalf("notification did not describe direct result: %q", notification)
	}
	if notificationEventID == "" {
		if _, unavailable := runner.Store.DecisionByID(context.Background(), "probe"); !errors.Is(unavailable, history.ErrAnalyticsUnavailable) && !errors.Is(unavailable, history.ErrDecisionNotFound) {
			t.Fatalf("missing notification decision id with analytics error %v", unavailable)
		}
		return
	}
	decision, err := runner.Store.DecisionByID(context.Background(), notificationEventID)
	if err != nil || decision.Action != "switched" || decision.Before != "a" || decision.After != "b" || len(decision.Followers) != 1 || decision.Followers[0].After != "b" {
		t.Fatalf("linked decision = %+v, %v", decision, err)
	}
}

func TestApplyPreservesConcurrentManualChanges(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.afterSelect = func(group, _ string) {
		if group == "warp" {
			direct := fixture.proxies["direct"]
			direct.Now = "c"
			fixture.proxies["direct"] = direct
		}
	}

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", Direct: "direct"}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if err != nil || selected != "b" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["direct"].Now != "c" {
		t.Fatalf("manual direct selection overwritten: %q", fixture.proxies["direct"].Now)
	}
	if got := strings.Join(fixture.writes, ","); got != "warp=b" {
		t.Fatalf("unexpected writes %q", got)
	}
}

func TestApplyDoesNotClaimASelectionChangedDuringValidation(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.afterSelect = func(group, _ string) {
		if group == "warp" {
			warp := fixture.proxies["warp"]
			warp.Now = "c"
			fixture.proxies["warp"] = warp
		}
	}

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", Direct: "direct"}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if selected != "" || !errors.Is(err, ErrManualOverride) {
		t.Fatalf("Apply = %q, %v; want manual override", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "c" || fixture.proxies["direct"].Now != "a" {
		t.Fatalf("manual selection not preserved: warp=%q direct=%q", fixture.proxies["warp"].Now, fixture.proxies["direct"].Now)
	}
}

func TestApplyRejectsStaleExpectedSelection(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)

	_, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain"}, "stale", []model.Sample{{Node: "b", Mbps: 80}})
	if !errors.Is(err, ErrManualOverride) {
		t.Fatalf("error %v does not wrap ErrManualOverride", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("stale apply wrote %v", fixture.writes)
	}
}

func TestApplyRejectsLiveChainDialerMismatch(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.mu.Lock()
	chain := fixture.proxies["chain"]
	chain.Dialer = "some-other-group"
	fixture.proxies["chain"] = chain
	fixture.mu.Unlock()

	_, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if err == nil || !strings.Contains(err.Error(), "refusing to change") {
		t.Fatalf("Apply error = %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("dialer mismatch wrote %v", fixture.writes)
	}
}

func TestApplyKeepsOriginalWhenItRanksFirst(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "a", Mbps: 80}, {Node: "b", Mbps: 100}})
	if err != nil || selected != "a" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("sticky result wrote %v", fixture.writes)
	}
}

func TestApplyTriesNextCandidateWhenTopOriginalChainFails(t *testing.T) {
	fixture, server := newControllerFixture(t)
	fixture.failChainOnNode = "a"
	runner := testRunner(t, fixture, server)

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "a", Mbps: 100}, {Node: "b", Mbps: 90}})
	if err != nil || selected != "b" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if got := strings.Join(fixture.writes, ","); got != "warp=b" {
		t.Fatalf("fallback candidate writes %q", got)
	}
}

func TestApplyStopsAtOriginalAfterBetterCandidateFails(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.failing["b"] = true

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "b", Mbps: 100}, {Node: "a", Mbps: 90}, {Node: "c", Mbps: 80}})
	if err != nil || selected != "a" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("worse fallback was selected: %v", fixture.writes)
	}
}

func TestApplyRestoresOriginalAfterTrialValidationFails(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.failChainOnNode = "b"

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "b", Mbps: 100}, {Node: "a", Mbps: 90}, {Node: "c", Mbps: 80}})
	if err != nil || selected != "a" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "a" {
		t.Fatalf("trial selection was stranded at %q", fixture.proxies["warp"].Now)
	}
	if got := strings.Join(fixture.writes, ","); got != "warp=b,warp=a" {
		t.Fatalf("restore writes %q", got)
	}
}

func TestApplyHonorsMinimumBandwidth(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", MinMbps: 50}, "a", []model.Sample{{Node: "b", Mbps: 40}, {Node: "a", Mbps: 30}})
	if err == nil || selected != "" {
		t.Fatalf("Apply = %q, %v; want no eligible candidate", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.writes) != 0 {
		t.Fatalf("below-minimum candidate wrote %v", fixture.writes)
	}
}

func TestApplyLeavesBelowMinimumOriginalForEligibleCandidate(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", MinMbps: 50}, "a", []model.Sample{{Node: "a", Mbps: 49}, {Node: "b", Mbps: 55}})
	if err != nil || selected != "b" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
}

func TestApplyRollsBackAfterCancellation(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.afterSelect = func(group, _ string) {
		if group == "warp" {
			cancel()
		}
	}

	selected, err := runner.Apply(ctx, config.Group{Name: "warp", Chain: "chain"}, "a", []model.Sample{{Node: "b", Mbps: 80}})
	if selected != "" || err == nil {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "a" {
		t.Fatalf("cancelled apply left %q selected", fixture.proxies["warp"].Now)
	}
}

func TestApplyTriesNextCandidateAndStrictlyRollsBack(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	fixture.failing["b"] = true

	selected, err := runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", StrictGeo: true}, "a", []model.Sample{{Node: "b", Mbps: 90}, {Node: "c", Mbps: 80}})
	if err != nil || selected != "c" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}

	fixture.mu.Lock()
	fixture.failing["b"] = false
	fixture.failing["chain"] = true
	fixture.writes = nil
	proxy := fixture.proxies["warp"]
	proxy.Now = "a"
	fixture.proxies["warp"] = proxy
	fixture.mu.Unlock()
	_, err = runner.Apply(context.Background(), config.Group{Name: "warp", Chain: "chain", StrictGeo: true}, "a", []model.Sample{{Node: "b", Mbps: 90}, {Node: "c", Mbps: 80}})
	if err == nil {
		t.Fatal("strict all-dead apply succeeded")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "a" {
		t.Fatalf("strict failure left selection at %q", fixture.proxies["warp"].Now)
	}
	if got := strings.Join(fixture.writes, ","); got != "warp=b,warp=c,warp=a" {
		t.Fatalf("rollback writes %q", got)
	}
}

func TestApplyGeoFallbackIsConfigurable(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) {
		return probe.GeoResult{Domain: ".google.com.hk", IP: "203.0.113.8", Colo: "HKG", Loc: "HK"}, nil
	}
	group := config.Group{Name: "warp", Chain: "chain", GeoProxy: "http://127.0.0.1:1", ExpectedGeo: ".google.com"}

	selected, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Mbps: 90}, {Node: "c", Mbps: 80}})
	if err != nil || selected != "b" {
		t.Fatalf("permissive Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	if got := strings.Join(fixture.writes, ","); got != "warp=b,warp=c,warp=b" {
		fixture.mu.Unlock()
		t.Fatalf("permissive geo fallback writes %q", got)
	}
	proxy := fixture.proxies["warp"]
	proxy.Now = "a"
	fixture.proxies["warp"] = proxy
	fixture.writes = nil
	fixture.mu.Unlock()

	group.StrictGeo = true
	selected, err = runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Mbps: 90}, {Node: "c", Mbps: 80}})
	if selected != "" || err == nil {
		t.Fatalf("strict Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "a" {
		t.Fatalf("strict geo failure left %q selected", fixture.proxies["warp"].Now)
	}
	if got := strings.Join(fixture.writes, ","); got != "warp=b,warp=c,warp=a" {
		t.Fatalf("strict geo rollback writes %q", got)
	}
}

func TestApplyIgnoresTraceTelemetryFailureAfterGoogleRegionPasses(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) {
		return probe.GeoResult{Domain: ".google.com", OK: true}, errors.New("trace unavailable")
	}
	group := config.Group{Name: "warp", Chain: "chain", GeoProxy: "http://127.0.0.1:1", ExpectedGeo: ".google.com", StrictGeo: true}

	selected, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Mbps: 90}})
	if err != nil || selected != "b" {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
}

func TestApplyRollsBackOnGeoRouteProofFailure(t *testing.T) {
	fixture, server := newControllerFixture(t)
	runner := testRunner(t, fixture, server)
	runner.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) {
		return probe.GeoResult{}, probe.ErrRoute
	}
	group := config.Group{Name: "warp", Chain: "chain", GeoProxy: "http://127.0.0.1:1", ExpectedGeo: ".google.com"}

	selected, err := runner.Apply(context.Background(), group, "a", []model.Sample{{Node: "b", Mbps: 90}})
	if selected != "" || !errors.Is(err, probe.ErrRoute) {
		t.Fatalf("Apply = %q, %v", selected, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.proxies["warp"].Now != "a" {
		t.Fatalf("route proof failure stranded %q", fixture.proxies["warp"].Now)
	}
}
