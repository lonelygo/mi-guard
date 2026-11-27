package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
)

const bytesPerMiB = 1 << 20

func mibPerSecond(mbps float64) float64 { return mbps * 1e6 / (8 * bytesPerMiB) }

func byteCount(n int64) string {
	switch {
	case n >= bytesPerMiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/bytesPerMiB)
	case n >= 1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// A provider name or controller error may contain terminal control characters.
func displayText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

func rankBenchResults(rows []model.Sample) {
	slices.SortStableFunc(rows, func(a, b model.Sample) int {
		if a.Mbps > b.Mbps {
			return -1
		}
		if a.Mbps < b.Mbps {
			return 1
		}
		return strings.Compare(a.Node, b.Node)
	})
}

// Redirected stdout retains the column order, units and full names used by scripts.
func printResults(out io.Writer, in []model.Sample) {
	if len(in) == 0 {
		return
	}
	rows := slices.Clone(in)
	rankBenchResults(rows)
	const row = "%9s  %7s  %7s  %8s  %10s  %-9s  %s\n"
	fmt.Fprintf(out, row, "Mbps", "MiB/s", "HTTP ms", "FAIL/TRY", "READ", "TYPE", "NODE / RESULT")
	for _, s := range rows {
		mbps, mib, latency := "-", "-", "-"
		if s.Mbps > 0 && s.Error == "" {
			mbps = fmt.Sprintf("%.3f", s.Mbps)
			mib = fmt.Sprintf("%.2f", mibPerSecond(s.Mbps))
		}
		if s.LatencyMS > 0 || s.Attempts >= 2 {
			latency = fmt.Sprint(s.LatencyMS)
		}
		name := displayText(s.Node)
		if s.Error != "" {
			name += "  · " + displayText(s.Error)
		}
		fmt.Fprintf(out, row, mbps, mib, latency, fmt.Sprintf("%d/%d", s.Failures, s.Attempts), byteCount(s.Bytes), displayText(s.ProxyType), name)
	}
}

func printLatencyGate(out io.Writer, group string, gate policy.LatencyGate, samples []model.Sample) {
	label := "延迟筛选 " + displayText(group)
	if gate.Factor == 0 {
		fmt.Fprintf(out, "%s · 已关闭\n", label)
		return
	}
	threshold, enabled := gate.Threshold()
	if !enabled {
		if gate.Samples > 0 {
			fmt.Fprintf(out, "%s · 历史均值 %.1f ms，比例门槛不可用，本轮不剔除\n", label, gate.MeanMS)
		} else {
			fmt.Fprintf(out, "%s · 冷启动，尚无本组历史延迟，本轮不剔除\n", label)
		}
		return
	}
	fmt.Fprintf(out, "%s · 历史均值 %.1f ms · %d 次有效探测 · %.2f 倍阈值 %.1f ms\n",
		label, gate.MeanMS, gate.Samples, gate.Factor, threshold)
	excluded, measured := 0, 0
	for _, sample := range samples {
		if !policy.HasBandwidth(sample) {
			continue
		}
		measured++
		reason := gate.Exclusion(sample)
		if reason == "" {
			continue
		}
		excluded++
		if latency, ok := sample.ProbeLatency(); ok {
			fmt.Fprintf(out, "  剔除 %s · %d ms\n", displayText(sample.Node), latency)
		} else {
			fmt.Fprintf(out, "  剔除 %s · 本轮 HTTP 延迟缺失\n", displayText(sample.Node))
		}
	}
	if measured == 0 {
		fmt.Fprintln(out, "  本轮没有测速成功的候选")
	} else if excluded == 0 {
		fmt.Fprintf(out, "  %d 个测速成功的候选通过延迟筛选\n", measured)
	}
}
