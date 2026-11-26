package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
)

func TestBenchOutputShowsComparableUnitsAndMissingMeasurements(t *testing.T) {
	good := model.Sample{Node: "🇺🇸 示例", ProxyType: "Trojan", Mbps: 13.310, Bytes: 4 << 20, LatencyMS: 320, Attempts: 2}
	bad := model.Sample{Node: "慢\n节点", Error: "204 probe: HTTP 504", Attempts: 1, Failures: 1}
	var output bytes.Buffer
	printResults(&output, []model.Sample{bad, good})
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header and two rows: %q", output.String())
	}
	if got := strings.Join(strings.Fields(lines[1]), " "); got != "13.310 1.59 320 0/2 4.00 MiB Trojan 🇺🇸 示例" {
		t.Fatalf("successful result: %s", got)
	}
	if got := strings.Join(strings.Fields(lines[2]), " "); got != "- - - 1/1 0 B 慢 节点 · 204 probe: HTTP 504" {
		t.Fatalf("failed result: %s", got)
	}
}

func TestLiveBenchClipsActiveLineAndPrintsFullFinishedNode(t *testing.T) {
	var output bytes.Buffer
	const columns = 60
	p := newBenchProgress(&output, columns)
	node := strings.Repeat("🇺🇸 示例", 18)
	p.update(guard.ScanProgress{Node: node, Total: 1})
	p.mu.Lock()
	first := strings.TrimPrefix(output.String(), "\r\x1b[2K")
	p.mu.Unlock()
	if !utf8.ValidString(first) || len(first) >= columns || !strings.HasSuffix(first, "...") {
		t.Errorf("invalid transient progress line at %d columns: %q", columns, first)
	}
	s := model.Sample{Node: node, Mbps: 8}
	p.update(guard.ScanProgress{Node: node, Total: 1, Completed: 1, Result: &s})
	p.close()
	if !strings.Contains(output.String(), node) {
		t.Fatal("full node name missing from settled result")
	}
}

func TestBenchProgressVisibleBeforeFirstResultAndPlainInPipe(t *testing.T) {
	var output bytes.Buffer
	p := newBenchProgress(&output, 0)
	p.update(guard.ScanProgress{Node: "示例", Total: 1})
	for _, want := range []string{"0/1", "测速中 示例"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("first progress: %q", output.String())
		}
	}
	s := model.Sample{Node: "示例", Error: "204 probe: HTTP 504"}
	p.update(guard.ScanProgress{Node: s.Node, Total: 1, Completed: 1, Result: &s})
	p.close()
	for _, want := range []string{"1/1 100%", "ETA 0s", s.Error} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("final progress: %q", output.String())
		}
	}
	if strings.ContainsAny(output.String(), "\r\x1b") {
		t.Fatalf("terminal escapes in pipe: %q", output.String())
	}
}

func TestBenchLatencyGateExplanation(t *testing.T) {
	valid := model.Sample{Node: "steady", Kind: "bench", Mbps: 80, LatencyMS: 150, Attempts: 2}
	outlier := model.Sample{Node: "slow\nnode", Kind: "bench", Mbps: 100, LatencyMS: 151, Attempts: 2}
	unknown := model.Sample{Node: "unknown", Kind: "bench", Mbps: 90, Attempts: 1}
	gate := policy.LatencyGate{MeanMS: 100, Samples: 12, Factor: 1.5}
	var output bytes.Buffer
	printLatencyGate(&output, "group", gate, []model.Sample{valid, outlier, unknown})
	for _, want := range []string{
		"延迟筛选 group · 历史均值 100.0 ms · 12 次有效探测 · 1.50 倍阈值 150.0 ms",
		"剔除 slow node · 151 ms", "剔除 unknown · 本轮 HTTP 延迟缺失",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("latency explanation %q is missing %q", output.String(), want)
		}
	}
	if strings.Contains(output.String(), "steady") {
		t.Fatalf("eligible node printed as excluded: %q", output.String())
	}
	output.Reset()
	printLatencyGate(&output, "group", gate, []model.Sample{{Node: "unreachable", Kind: "bench", Error: "204 probe: HTTP 504"}})
	if !strings.Contains(output.String(), "没有测速成功的候选") {
		t.Fatalf("failed probes presented as passing: %q", output.String())
	}
}

func TestBenchLatencyGateColdStartAndDisabled(t *testing.T) {
	for _, test := range []struct {
		gate policy.LatencyGate
		want string
	}{
		{gate: policy.LatencyGate{Factor: 1.5}, want: "冷启动"},
		{gate: policy.LatencyGate{}, want: "已关闭"},
		{gate: policy.LatencyGate{MeanMS: 0, Samples: 4, Factor: 1.5}, want: "比例门槛不可用"},
	} {
		var output bytes.Buffer
		printLatencyGate(&output, "group", test.gate, nil)
		if !strings.Contains(output.String(), test.want) {
			t.Errorf("latency explanation %q is missing %q", output.String(), test.want)
		}
	}
}
