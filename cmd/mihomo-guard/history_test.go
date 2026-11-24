package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/model"
)

func captureHistory(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = stdout }()
	runErr := run(context.Background(), append([]string{"history"}, args...))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data, runErr
}

func TestHistoryDefaultConfigAndMissingDataAreReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := config.DefaultPath()
	if out, err := captureHistory(t); !errors.Is(err, os.ErrNotExist) || len(out) != 0 || strings.Contains(err.Error(), home) {
		t.Fatalf("missing guard: %q, %v", out, err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("missing guard created files: %v, %v", entries, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("groups: []\nspeedtest: {monthly_bytes: 101}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureHistory(t)
	var got DashboardSnapshot
	if err != nil || json.Unmarshal(out, &got) != nil || got.BudgetBytes != 101 || got.UsedBytes != 0 || got.PendingBytes != 0 ||
		got.LastSweep != nil || got.Samples == nil || len(got.Samples) != 0 || got.Groups == nil || len(got.Groups) != 0 {
		t.Fatalf("empty snapshot: %s, %v", out, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("empty ledger created files: %v, %v", entries, err)
	}
	// A selector can be observed before the first budgeted transfer.
	sidecar := `{"old":{"node":"leaf","since":"2024-01-01T00:00:00Z","last_seen":"2024-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, "speed-history.json.selections.json"), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = captureHistory(t)
	if err != nil || json.Unmarshal(out, &got) != nil || got.UsedBytes != 0 || len(got.Groups) != 1 ||
		got.Groups[0].Configured || got.Groups[0].Node != "leaf" || got.Groups[0].LongestSeconds != 0 {
		t.Fatalf("selection-only snapshot: %s, %v", out, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("selection-only read created files: %v, %v", entries, err)
	}
}

func TestHistoryPreservesConciseProbeStatus(t *testing.T) {
	for raw, want := range map[string]string{
		"204 probe: HTTP 504":                              "HTTP 504",
		"204 probe: HTTP 504 https://example.test/private": "failed",
		"unexpected EOF":                                   "failed",
	} {
		if got := dashboardSample(model.Sample{Error: raw}).Error; got != want {
			t.Errorf("error label %q = %q, want %q", raw, got, want)
		}
	}
}

func TestHistoryExportsRecentAndRequestedDecisionWithoutDependingOnAnalytics(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "guard.yaml")
	if err := os.WriteFile(configPath, []byte("history: {file: history.json}\ngroups: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := history.New(filepath.Join(dir, "history.json"), 50, 1)
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 65; i++ {
		threshold := 80.0
		decision := history.Decision{ID: fmt.Sprintf("decision-%02d", i), At: base.Add(time.Duration(i) * time.Minute), Source: "manual",
			Group: "US-WARP", Before: "node-a", After: "node-b", Action: "switched", Trigger: "manualBench",
			NearBestPct: 20, ThresholdMbps: &threshold, Candidates: []history.DecisionCandidate{{Node: "node-b", Mbps: 100, Streams: 4, RequestedBytes: 16 << 20, Selected: true}}}
		if err := store.RecordDecision(context.Background(), decision); errors.Is(err, history.ErrAnalyticsUnavailable) {
			t.Skip("SQLite decision persistence is unavailable without cgo")
		} else if err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := writeDashboardHistoryForDecision(&out, configPath, "decision-00"); err != nil {
		t.Fatal(err)
	}
	var got DashboardSnapshot
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Decisions) != 60 || got.Decisions[0].ID != "decision-64" || got.SelectedDecision == nil || got.SelectedDecision.ID != "decision-00" || got.DecisionWarning != "" {
		t.Fatalf("decision export = recent %d first %+v selected %+v warning %q", len(got.Decisions), got.Decisions[0], got.SelectedDecision, got.DecisionWarning)
	}
	out.Reset()
	var missing DashboardSnapshot
	if err := writeDashboardHistoryForDecision(&out, configPath, "expired-decision"); err != nil || json.Unmarshal(out.Bytes(), &missing) != nil ||
		missing.SelectedDecision != nil || !strings.Contains(missing.DecisionWarning, "已过期或不存在") || len(missing.Samples) != 0 || missing.Groups == nil {
		t.Fatalf("missing decision fallback = %s, %v", out.String(), err)
	}

	if err := os.WriteFile(filepath.Join(dir, "broken.json.analytics.sqlite3"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenConfig := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(brokenConfig, []byte("history: {file: broken.json}\ngroups: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	var broken DashboardSnapshot
	if err := writeDashboardHistory(&out, brokenConfig); err != nil || json.Unmarshal(out.Bytes(), &broken) != nil ||
		broken.DecisionWarning == "" || len(broken.Samples) != 0 || broken.Groups == nil {
		t.Fatalf("corrupt analytics fallback = %s, %v", out.String(), err)
	}
}

func TestHistoryExportsConfiguredBudgetProfilesAndObservedTenure(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("config", 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("config", "guard.yaml")
	const yaml = `mihomo: {config_path: absent-core.yaml, secret: controller-secret}
speedtest: {monthly_bytes: 9000000000, billing_day: 15, url: "https://private.example/subscription?token=secret"}
history: {file: records/custom.json, samples_per_node: 2}
groups: [{name: primary, chain: "Exit / JP"}, {name: standalone, mode: selector}]
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureHistory(t, "-c", path)
	var got DashboardSnapshot
	if err != nil || json.Unmarshal(out, &got) != nil || len(got.Groups) != 2 || got.Groups[0].Chain != "Exit / JP" ||
		!got.Groups[1].Configured || got.Groups[1].Name != "standalone" || got.Groups[1].Node != "" || got.Groups[1].Since != nil {
		t.Fatalf("unobserved configured groups: %s, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join("config", "records")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history created the configured directory: %v", err)
	}
	ctx := context.Background()
	store := history.New(filepath.Join("config", "records", "custom.json"), 2, 15)
	at := time.Now().UTC().Truncate(time.Second).Add(-72*time.Hour + 123456789*time.Nanosecond)
	const target = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const rawError = "Get https://private.example/subscription?token=secret: dial tcp 192.0.2.80:443"
	sample := model.Sample{At: at, Node: "HK/01", Scope: "front", Kind: "bench", Bytes: 11}
	settle := func(sample model.Sample) {
		t.Helper()
		r, err := store.Reserve(ctx, 64, 9000000000)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, r, sample); err != nil {
			t.Fatal(err)
		}
	}
	settle(sample) // This sample falls outside the retention window.
	sample.At, sample.Bytes, sample.Mbps = at.Add(time.Hour), 23, 16.25
	settle(sample)
	sample.At, sample.Scope = at.Add(2*time.Hour), "chain:Exit / JP"
	sample.Bytes, sample.Mbps, sample.LatencyMS, sample.Attempts, sample.Failures = 7, 0, 12, 2, 1
	sample.Profile = model.DownloadProfile{Streams: 4, TotalBytes: 64, Target: target}
	sample.Error = rawError + " at " + root
	settle(sample)
	discard, err := store.Reserve(ctx, 16, 9000000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(ctx, discard, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(ctx, 40, 9000000000); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSweep(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, model.Sample{Kind: "heartbeat", Scope: "primary", Error: rawError}); err != nil {
		t.Fatal(err)
	}
	observe := func(group, node string, when time.Time, switched bool) {
		t.Helper()
		if _, err := store.RecordSelection(ctx, group, node, when, switched); err != nil {
			t.Fatal(err)
		}
	}
	observe("primary", "earlier", at, false)
	observe("primary", "earlier", at.Add(2*time.Hour+500*time.Millisecond), false)
	since := at.Add(20 * time.Hour)
	lastSeen := since.Add(time.Hour)
	observe("primary", "HK/01", since, true)
	observe("primary", "HK/01", lastSeen, false)
	observe("retired", "https://private.example/old-node", at, false)
	out, err = captureHistory(t, "-c", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("expected one JSON object on stdout: %v", err)
	}
	if got.BudgetBytes != 9000000000 || got.UsedBytes != 86 || got.PendingBytes != 40 || got.LastSweep == nil || !got.LastSweep.Equal(at) ||
		got.CycleStart.Local().Day() != 15 || got.CycleStart.Local().Hour() != 0 || !got.CycleEnd.Equal(got.CycleStart.Local().AddDate(0, 1, 0)) {
		t.Fatalf("billing or cadence: %+v", got)
	}
	if len(got.Samples) != 2 || len(got.Groups) != 3 {
		t.Fatalf("expected two retained measurements and three groups: %+v", got)
	}
	legacy, failed := got.Samples[0], got.Samples[1]
	if !legacy.Time.Equal(at.Add(time.Hour)) || legacy.Mbps != 16.25 || legacy.Streams != 0 || legacy.RequestedBytes != 0 ||
		legacy.Target != "" || legacy.Bytes != 23 || legacy.Error != "" || legacy.Scope != "front" || legacy.Node != "HK/01" {
		t.Errorf("legacy measurement: %+v", legacy)
	}
	if failed.Streams != 4 || failed.RequestedBytes != 64 || failed.Target != target || failed.Scope != sample.Scope || failed.Bytes != 7 ||
		failed.Attempts != 2 || failed.Failures != 1 || failed.Error != "failed" || failed.LatencyMS != 12 {
		t.Errorf("profiled failure: %+v", failed)
	}
	group, retired := got.Groups[0], got.Groups[2]
	if !group.Configured || group.Name != "primary" || group.Chain != "Exit / JP" || group.Node != "HK/01" || !group.GuardSelected ||
		group.Since == nil || !group.Since.Equal(since) || group.LastSeen == nil || !group.LastSeen.Equal(lastSeen) ||
		group.LongestNode != "earlier" || group.LongestSeconds != 7200.5 {
		t.Errorf("bounded observed selection: %+v", group)
	}
	if retired.Name != "retired" || retired.Configured || retired.GuardSelected || retired.Node == "" || retired.LongestNode != "" || retired.LongestSeconds != 0 {
		t.Errorf("retired group: %+v", retired)
	}
	for _, private := range []string{root, "controller-secret", "private.example", "://", "token=", "192.0.2.80", "absent-core", "custom.json", "dial tcp"} {
		if bytes.Contains(out, []byte(private)) {
			t.Errorf("dashboard disclosed %q", private)
		}
	}
	var wire struct{ Samples, Groups []json.RawMessage }
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	checkKeys := func(object []byte, keys string) {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(object, &fields); err != nil {
			t.Fatal(err)
		}
		want := strings.Fields(keys)
		slices.Sort(want)
		if actual := slices.Sorted(maps.Keys(fields)); !slices.Equal(actual, want) {
			t.Errorf("wire keys %v, want %v", actual, want)
		}
	}
	checkKeys(out, "budgetBytes usedBytes pendingBytes cycleStart cycleEnd lastSweep samples groups decisions")
	checkKeys(wire.Samples[0], "time node provider scope kind mbps latencyMS bytes attempts failures error streams requestedBytes target")
	checkKeys(wire.Groups[1], "name chain configured node since lastSeen guardSelected longestNode longestSeconds")
}

func TestHistoryRejectsMalformedFilesWithoutOutputOrMutation(t *testing.T) {
	for _, tc := range []struct{ name, file, contents string }{
		{"config", "guard.yaml", "speedtest: {monthly_bytes: https://private.example/secret}"},
		{"ledger", "speed-history.json", "{broken"},
		{"sidecar", "speed-history.json.selections.json", `{"private.example":{"node":"leaf"}}`},
		{"null sidecar", "speed-history.json.selections.json", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "guard.yaml")
			if err := os.WriteFile(configPath, []byte("groups: []"), 0o600); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, tc.file)
			if err := os.WriteFile(file, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = writeDashboardHistory(&out, configPath)
			if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "private.example") || strings.Contains(err.Error(), dir) {
				t.Fatalf("invalid file: out=%q, err=%v", out.String(), err)
			}
			if got, err := os.ReadFile(file); err != nil || string(got) != tc.contents {
				t.Fatalf("invalid file was changed: %q, %v", got, err)
			}
			if after, err := os.ReadDir(dir); err != nil || len(after) != len(before) {
				t.Fatalf("read created files: %v, %v", after, err)
			}
		})
	}
}
