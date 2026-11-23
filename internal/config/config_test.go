package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v2"
)

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
	if c.Selection.SevereDropPct != 20 || c.Selection.NearBestPct != 20 || c.Schedule.FullInterval != 12*time.Hour || c.Schedule.HeartbeatFailures != 2 || c.History.SamplesPerNode != 50 {
		t.Fatalf("unexpected defaults: %+v %+v", c.Selection, c.Schedule)
	}
	if c.History.File != filepath.Join(filepath.Dir(path), "speed-history.json") || !filepath.IsAbs(c.Mihomo.StableBinary) || c.Mihomo.AllowPrerelease {
		t.Fatalf("unexpected paths or release policy: %+v", c.Mihomo)
	}
	if c.Notifications.Helper != AutoNotificationHelper {
		t.Fatalf("notification helper = %q, want auto", c.Notifications.Helper)
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
		"mihomo: {controller: 'http://example.test:9090'}", "speedtest: {billing_day: 29}",
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
