package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/model"
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
