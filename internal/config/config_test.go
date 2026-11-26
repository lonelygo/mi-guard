package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v2"
)

func TestLoadGuardLeavesCoreSecretLookupToLiveCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "core.yaml"), []byte("secret: core-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "guard.yaml")
	if err := os.WriteFile(path, []byte("mihomo: {config_path: core.yaml}\nhistory: {file: records/speed.json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	offline, err := LoadGuard(path)
	if err != nil || offline.Mihomo.Secret != "" || offline.History.File != filepath.Join(dir, "records", "speed.json") {
		t.Fatalf("offline config: secret populated=%v, history=%q, err=%v", offline.Mihomo.Secret != "", offline.History.File, err)
	}
	live, err := Load(path)
	if err != nil || live.Mihomo.Secret != "core-only" {
		t.Fatalf("live config did not resolve the core secret: %v", err)
	}
}

func TestInitAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "guard.yaml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteExample(path); !os.IsExist(err) {
		t.Fatalf("overwriting init: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Selection.SevereDropPct != 20 || c.Selection.NearBestPct != 20 || c.Selection.LatencyOutlierFactor != 1.5 ||
		c.Schedule.FullInterval != 12*time.Hour || c.Schedule.HeartbeatFailures != 2 || c.History.SamplesPerNode != 50 {
		t.Fatalf("unexpected defaults: %+v %+v", c.Selection, c.Schedule)
	}
	if c.History.File != filepath.Join(filepath.Dir(path), "speed-history.json") || !filepath.IsAbs(c.Mihomo.StableBinary) || c.Mihomo.AllowPrerelease {
		t.Fatalf("unexpected paths or release policy: %+v", c.Mihomo)
	}
	if c.Notifications.Helper != AutoNotificationHelper {
		t.Fatalf("notification helper = %q, want auto", c.Notifications.Helper)
	}
	if limit, err := c.Speed.BenchTotalBytes(); err != nil || limit != 16<<20 || c.Speed.BenchStreams != 4 || c.Speed.SweepBytes != 4<<20 {
		t.Fatalf("manual cap = %d, speeds = %+v, err = %v", limit, c.Speed, err)
	}
}

func TestManualStreamConfigUpgrade(t *testing.T) {
	for _, tc := range []struct {
		settings string
		streams  int
	}{
		{"streams_per_node: 1", 4},
		{"streams_per_node: 3", 3},
		{"streams_per_node: 3, bench_streams: 1", 1},
		{"bench_streams: 2", 2},
	} {
		path := filepath.Join(t.TempDir(), "guard.yaml")
		if err := os.WriteFile(path, []byte("speedtest: {"+tc.settings+", sweep_bytes: 128}"), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil || c.Speed.BenchStreams != tc.streams || c.Speed.BenchBytesPerStream != 4<<20 || c.Speed.SweepBytes != 128 {
			t.Errorf("upgrade %q: got %+v, %v", tc.settings, c.Speed, err)
		}
	}
}

func TestDownloadProfileTracksMeasurementSettings(t *testing.T) {
	cfg := Config{Speed: Speed{URL: "https://probe.test/data?token=secret", Timeout: time.Second, LivenessURL: "https://probe.test/204"}, Schedule: Schedule{HeartbeatTimeout: time.Second}}
	p := cfg.DownloadProfile(4, 16<<20)
	if !p.Known() || p.Streams != 4 || p.TotalBytes != 16<<20 || len(p.Target) != 64 || strings.Contains(p.Target, "secret") {
		t.Fatalf("invalid download profile %+v", p)
	}
	if single := cfg.DownloadProfile(1, 4<<20); single == p || single.Target != p.Target {
		t.Fatalf("same endpoints should share a target fingerprint: %+v, %+v", p, single)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Speed.URL += "2" }, func(c *Config) { c.Speed.LivenessURL += "2" },
		func(c *Config) { c.Speed.Timeout++ }, func(c *Config) { c.Schedule.HeartbeatTimeout++ },
	} {
		other := cfg
		change(&other)
		if other.DownloadProfile(4, 16<<20) == p {
			t.Errorf("endpoint or timeout change kept stale profile: %+v", other)
		}
	}
}

func TestLatencyProbeKeyTracksOnlyThe204Probe(t *testing.T) {
	cfg := Config{Speed: Speed{
		URL: "https://download.test/data?token=secret", LivenessURL: "https://probe.test/204?token=private", Timeout: time.Second,
	}, Schedule: Schedule{HeartbeatTimeout: 3 * time.Second}}
	key := cfg.LatencyProbeKey()
	if len(key) != 64 || strings.Contains(key, "private") {
		t.Fatalf("invalid latency probe key %q", key)
	}
	other := cfg
	other.Speed.URL += "2"
	other.Speed.Timeout++
	other.Speed.BenchStreams = 8
	if other.LatencyProbeKey() != key {
		t.Fatal("download-only settings split HTTP latency history")
	}
	other.Speed.LivenessURL += "2"
	if other.LatencyProbeKey() == key {
		t.Fatal("liveness URL change kept stale latency history")
	}
	other = cfg
	other.Schedule.HeartbeatTimeout++
	if other.LatencyProbeKey() == key {
		t.Fatal("probe timeout change kept stale latency history")
	}
}

func TestLatencyOutlierFactorCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.yaml")
	if err := os.WriteFile(path, []byte("selection: {latency_outlier_factor: 0}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadGuard(path)
	if err != nil || c.Selection.LatencyOutlierFactor != 0 {
		t.Fatalf("disabled latency factor = %v, err=%v", c.Selection.LatencyOutlierFactor, err)
	}
}

func TestDefaultReferenceUsesPublicPlaceholders(t *testing.T) {
	var c Config
	if err := yaml.UnmarshalStrict(example, &c); err != nil {
		t.Fatal(err)
	}
	if c.Mihomo.ConfigPath != "path_to_your_config.yaml" || c.Mihomo.StableBinary != "path_to_your_mihomo_core" {
		t.Fatalf("default paths = %q, %q", c.Mihomo.ConfigPath, c.Mihomo.StableBinary)
	}
	want := []Group{
		{Name: "primary-group-1", Chain: "dialer-proxy-1", Direct: "direct-group-1"},
		{Name: "primary-group-2", Chain: "dialer-proxy-2", Direct: "direct-group-2"},
	}
	if len(c.Groups) != len(want) {
		t.Fatalf("default groups = %+v", c.Groups)
	}
	for i := range want {
		if c.Groups[i].Name != want[i].Name || c.Groups[i].Chain != want[i].Chain || c.Groups[i].Direct != want[i].Direct {
			t.Fatalf("default group %d = %+v, want names %+v", i, c.Groups[i], want[i])
		}
	}

	overlay, err := ReadCoreFile(filepath.Join("..", "..", "share", "mi-guard", "mihomo-overlay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay.CheckBench(c.Mihomo); err != nil {
		t.Fatalf("shipped overlay does not match default bench wiring: %v", err)
	}
	geoListeners := 0
	for _, g := range c.Groups {
		if g.GeoProxy == "" {
			continue
		}
		geoListeners++
		if err := overlay.checkListener(g.GeoProxy, g.ProbeTarget()); err != nil {
			t.Fatalf("shipped overlay does not match geo wiring for %q: %v", g.Name, err)
		}
	}
	if geoListeners == 0 {
		t.Fatal("default reference has no enabled geo listener to cross-check")
	}
}

func TestNotificationHelperExplicitPathStillResolvesFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "guard.yaml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "helper: auto", "helper: helpers/custom.app", 1))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(path), "helpers", "custom.app")
	if c.Notifications.Helper != want {
		t.Fatalf("notification helper = %q, want %q", c.Notifications.Helper, want)
	}
}

func TestLegacyGeneratedNotificationHelperExpandsForCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.yaml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "helper: auto", "helper: ~/Applications/mi-guard.app", 1))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Applications", "mi-guard.app")
	if c.Notifications.Helper != want {
		t.Fatalf("legacy notification helper = %q, want %q", c.Notifications.Helper, want)
	}
}

func TestInvalidGuardConfiguration(t *testing.T) {
	for _, content := range []string{
		"selection: {severe_drop_pct: 100}", "selection: {severe_drop_pct: 0}", "unknown_field: true",
		"selection: {latency_outlier_factor: 0.99}", "selection: {latency_outlier_factor: -.inf}", "selection: {latency_outlier_factor: .nan}",
		"mihomo: {controller: 'http://example.test:9090'}", "speedtest: {billing_day: 29}",
		"speedtest: {bench_streams: 0}", "speedtest: {bench_streams: 9}", "speedtest: {streams_per_node: 0}",
		"speedtest: {streams_per_node: 3, bench_streams: 0}", "speedtest: {bench_bytes_per_stream: 0}",
		"speedtest: {bench_bytes_per_stream: 9223372036854775807}",
		"groups: [{name: mi-guard-bench, chain: other}]",
		"groups: [{name: TW}]", "groups: [{name: TW, mode: chain}]",
		"groups: [{name: TW, mode: selector, chain: Exit}]", "groups: [{name: TW, mode: selector, direct: Mirror}]",
		"groups: [{name: TW, mode: typo}]", "groups: [{name: mi-guard-bench, mode: selector}]",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("accepted invalid config: %s", content)
		}
	}
}

func TestCustomGroupsReplaceDefaultsAndKeepLegacyChainMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.yaml")
	custom := "groups:\n  - {name: HK, chain: HK-exit}\n  - {name: TW, mode: selector}\n"
	if err := os.WriteFile(path, []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Groups) != 2 || c.Groups[0].ProbeTarget() != "HK-exit" || c.Groups[1].ProbeTarget() != "TW" {
		t.Fatalf("loaded incorrect routes: %+v", c.Groups)
	}
	for _, g := range c.Groups {
		if g.Direct != "" || g.GeoProxy != "" || g.ExpectedGeo != "" {
			t.Fatalf("custom group inherited a default group's wiring: %+v", g)
		}
	}
}

func TestStandaloneSelectorWiring(t *testing.T) {
	var f CoreFile
	data := `proxy-groups:
  - {name: TW, type: select}
  - {name: Auto, type: url-test}
listeners:
  - {name: tw-check, type: mixed, listen: 127.0.0.1, port: 7900, proxy: TW}
`
	if err := yaml.Unmarshal([]byte(data), &f); err != nil {
		t.Fatal(err)
	}
	g := Group{Name: "TW", Mode: GroupModeSelector, GeoProxy: "http://127.0.0.1:7900"}
	if err := f.CheckManaged([]Group{g}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Auto", "Missing"} {
		if err := f.CheckManaged([]Group{{Name: name, Mode: GroupModeSelector}}); err == nil {
			t.Errorf("accepted %q as a manual selector", name)
		}
	}
	f.Listeners[0].Proxy = "Another selector"
	if err := f.CheckManaged([]Group{g}); err == nil {
		t.Fatal("accepted geo listener routed to another selector")
	}
}

// Every accepted YAML field should appear in the reference, including options
// with zero defaults. One group may document fields omitted by another group.
func TestDefaultReferenceListsEveryOption(t *testing.T) {
	schema, err := yaml.Marshal(Config{Groups: []Group{{}}})
	if err != nil {
		t.Fatal(err)
	}
	paths := func(document []byte) map[string]bool {
		var root any
		if err := yaml.Unmarshal(document, &root); err != nil {
			t.Fatal(err)
		}
		found := make(map[string]bool)
		var visit func(string, any)
		visit = func(path string, value any) {
			switch v := value.(type) {
			case map[any]any:
				for key, child := range v {
					visit(path+"."+key.(string), child)
				}
			case []any:
				for _, item := range v {
					visit(path+"[]", item)
				}
			default:
				found[path] = true
			}
		}
		visit("", root)
		return found
	}
	documented := paths(example)
	for field := range paths(schema) {
		if !documented[field] {
			t.Errorf("missing configuration reference entry for %s", strings.TrimPrefix(field, "."))
		}
	}
}

func TestDedicatedListenerOwnership(t *testing.T) {
	data := `listeners:
  - {name: measurement, type: mixed, listen: 127.0.0.1, port: 17987, proxy: bench}
proxy-groups:
  - {name: bench, type: select}
`
	cfg := Core{TestGroup: "bench", TestProxy: "http://127.0.0.1:17987"}
	for _, bad := range []string{"", "  - {name: alongside, type: socks, listen: 127.0.0.1, port: 18000, proxy: bench}\n"} {
		path := filepath.Join(t.TempDir(), "core.yaml")
		if err := os.WriteFile(path, []byte(strings.Replace(data, "proxy-groups:", bad+"proxy-groups:", 1)), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := ReadCoreFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if e := f.CheckBench(cfg); (e == nil) != (bad == "") {
			t.Errorf("extra listener %q, err=%v", bad, e)
		}
	}
}
