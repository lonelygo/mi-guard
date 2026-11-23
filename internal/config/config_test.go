package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	for _, content := range []string{"selection: {severe_drop_pct: 100}", "selection: {severe_drop_pct: 0}", "unknown_field: true", "mihomo: {controller: 'http://example.test:9090'}", "speedtest: {billing_day: 29}", "groups: [{name: mi-guard-bench, chain: other}]"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("accepted invalid config: %s", content)
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
