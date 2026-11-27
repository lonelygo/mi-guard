package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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

func TestBenchRendersLiveRankingAndLeavesOneFinalTable(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var live, final bytes.Buffer
	p := newBenchProgress(&live, benchSize{60, 12})
	defer p.close()
	p.describe("测速组 🇺🇲 美国节点", "speed.example", 4, benchOptions{bytes: 16 << 20, bytesPerStream: 4 << 20}, 8*time.Second)
	long := strings.Repeat("🇺🇸 示例", 18)
	slow := model.Sample{Kind: "bench", Node: long, Mbps: 12, LatencyMS: 425, Attempts: 2}
	fast := model.Sample{Kind: "bench", Node: "🇺🇸 美国-US2", Mbps: 60, LatencyMS: 184, Attempts: 2}
	for i, sample := range []model.Sample{slow, fast} {
		p.update(guard.ScanProgress{Node: sample.Node, Completed: i, Total: 2})
		p.update(guard.ScanProgress{Node: sample.Node, Completed: i + 1, Total: 2, Result: &sample})
	}
	p.close()
	raw := live.String()
	if strings.Count(raw, "\x1b[?1049h") != 1 || strings.Count(raw, "\x1b[?1049l") != 1 ||
		!strings.Contains(raw, "正在测速") || strings.Contains(raw, long) || !strings.Contains(raw, "…") {
		t.Fatalf("invalid live screen lifecycle or truncation: %q", raw)
	}
	frame := p.frame(time.Second)
	if !strings.Contains(frame[len(frame)-2], fast.Node) || !strings.Contains(frame[len(frame)-1], "425 ms") {
		t.Fatalf("live ranking: %q", frame)
	}
	p.printFinal(&final, []model.Sample{slow, fast}, nil)
	for _, want := range []string{"4 流 × 4 MiB · 16 MiB/节点", "已完成 2/2", "节点名称", long, fast.Node} {
		if strings.Count(final.String(), want) != 1 {
			t.Errorf("expected %q once in transcript: %s", want, final.String())
		}
	}
	if strings.Contains(final.String(), "\x1b") || strings.Contains(final.String(), "正在读取") {
		t.Fatal("NO_COLOR transcript contains styling or a stale loading line")
	}
}

func TestBenchProgressVisibleBeforeFirstResultAndPlainInPipe(t *testing.T) {
	var output bytes.Buffer
	p := newBenchProgress(&output, benchSize{})
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

func TestBenchViewportAndPartialResults(t *testing.T) {
	p := &benchProgress{
		heading: []string{"测速组", "下载目标"}, start: time.Now(),
		last: guard.ScanProgress{Completed: 20, Total: 22, Node: "正在测量的节点"},
	}
	for i := 0; i < 20; i++ {
		p.ranked = append(p.ranked, model.Sample{Kind: "bench", Node: "美国-US2", Mbps: 60})
	}
	for _, size := range []benchSize{{18, 2}, {40, 6}, {80, 12}, {144, 30}} {
		p.size = size
		frame := p.frame(35 * time.Second)
		if len(frame) > size.rows {
			t.Fatalf("frame has %d rows for %v", len(frame), size)
		}
		for _, line := range frame {
			if cellWidth(line) >= size.columns {
				t.Errorf("live line wraps in %v: %q", size, line)
			}
		}
		if size.rows == 12 && !strings.Contains(frame[len(frame)-1], "另有") {
			t.Errorf("hidden results missing from %q", frame)
		}
	}
	var out bytes.Buffer
	p.printFinal(&out, p.ranked, context.Canceled)
	if !strings.Contains(out.String(), "已停止 20/22") || strings.Contains(out.String(), "剩余") || strings.Count(out.String(), "美国-US2") != 20 {
		t.Fatalf("partial transcript missing stopped status or results: %s", out.String())
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
