package main

import (
	"strings"
	"testing"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestTerminalWidthAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
	}{
		{"ASCII美国", 9}, {"🇺🇸", 2}, {"⛓️‍💥", 2}, {"e\u0301", 1},
		{"✈️", 2}, {"👍🏽", 2}, {"1️⃣", 2}, {"👩🏽‍💻", 2}, {"☕", 2}, {"Ａ", 2},
	} {
		if got := cellWidth(tc.text); got != tc.width {
			t.Errorf("%q occupies %d cells, want %d", tc.text, got, tc.width)
		}
		if got := fitCells(tc.text, tc.width); got != tc.text {
			t.Errorf("exact fit of %q produced %q", tc.text, got)
		}
		if got := fitCells(tc.text+"bc", tc.width+1); got != tc.text+"…" {
			t.Errorf("truncating after %q produced %q", tc.text, got)
		}
	}
	for _, cluster := range []string{"🇺🇸", "⛓️‍💥", "👩🏽‍💻", "1️⃣", "国"} {
		if got := fitCells("A"+cluster+"B", 3); got != "A…" {
			t.Errorf("split %q at the right edge: %q", cluster, got)
		}
	}
	if fitCells("美国", 0) != "" || fitCells("美国", 1) != "…" {
		t.Error("invalid result in zero- or one-cell field")
	}
}

func TestBenchTableChineseAndEmojiPositions(t *testing.T) {
	names := []string{"RL 🇺🇸 美国-US2", "⛓️‍💥 US-WARP", "TJ e\u0301", "TJ 美国1-TJ-US1"}
	for _, width := range []int{39, 55, 79, 103, 160} {
		table := newBenchTable(width, len(names), false)
		heading := table.header()
		// Compare the display-column right edge of the header and of every
		// numeric value. UTF-8 byte and rune offsets both vary across these rows.
		edge := func(line, word string) int {
			i := strings.Index(line, word)
			if i < 0 {
				t.Fatalf("field %q missing in %q", word, line)
			}
			return cellWidth(line[:i+len(word)])
		}
		for i, name := range names {
			s := model.Sample{Kind: "bench", Node: name, ProxyType: "Vless", LatencyMS: 184, Mbps: 60.143, Attempts: 2, Bytes: 16 << 20}
			line := table.sample(i+1, s)
			if edge(line, "7.17") != edge(heading, "↓") || cellWidth(line) != table.width {
				t.Errorf("speed shifted at width %d:\n%s\n%s", width, heading, line)
			}
			if table.fields[latencyCol] > 0 && edge(line, "184 ms") != edge(heading, "HTTP 延迟") {
				t.Errorf("latency shifted at width %d:\n%s\n%s", width, heading, line)
			}
		}
	}
}

func TestBenchFailedRowPreservesReasonWithoutControlCharacters(t *testing.T) {
	var out strings.Builder
	bad := model.Sample{Kind: "bench", Node: "🇺🇸 慢\n节点", Error: "204 probe: HTTP 504\x1b[2J", Attempts: 1, Failures: 1}
	newBenchTable(103, 1, false).print(&out, []model.Sample{bad})
	for _, want := range []string{"慢 节点", "1/1", "失败", "#1  204 probe: HTTP 504"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in failed result: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "0 ms") {
		t.Fatalf("unsafe control or invented latency: %q", out.String())
	}
}
