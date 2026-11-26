package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lonelygo/mi-guard/internal/guard"
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
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func printResults(out io.Writer, in []model.Sample) {
	if len(in) == 0 {
		return
	}
	rows := slices.Clone(in)
	slices.SortStableFunc(rows, func(a, b model.Sample) int {
		if a.Mbps > b.Mbps {
			return -1
		}
		if a.Mbps < b.Mbps {
			return 1
		}
		return strings.Compare(a.Node, b.Node)
	})
	// The variable-width Unicode node name is last so flags and CJK characters
	// cannot misalign the numeric columns in different terminals.
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

type benchProgress struct {
	out            io.Writer
	columns        int
	start          time.Time
	mu             sync.Mutex
	last           guard.ScanProgress
	lastCompletion time.Duration
	stop, stopped  chan struct{}
}

func newBenchProgress(out io.Writer, columns int) *benchProgress {
	p := &benchProgress{out: out, columns: columns, start: time.Now()}
	if columns > 0 {
		p.stop, p.stopped = make(chan struct{}), make(chan struct{})
		go p.tick()
	}
	return p
}

func stderrColumns() int {
	info, err := os.Stderr.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 || os.Getenv("TERM") == "dumb" {
		return 0
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stderr
	size, err := cmd.Output()
	if err != nil {
		return 0
	}
	var rows, cols int
	if _, err := fmt.Sscan(string(size), &rows, &cols); err != nil || cols < 40 {
		return 0
	}
	return cols
}

func (p *benchProgress) tick() {
	defer close(p.stopped)
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-timer.C:
			p.mu.Lock()
			if p.last.Total > 0 && p.last.Result == nil {
				p.draw()
			}
			p.mu.Unlock()
		}
	}
}

func (p *benchProgress) update(e guard.ScanProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = e
	if e.Result != nil {
		p.lastCompletion = time.Since(p.start)
	}
	p.draw()
}

func (p *benchProgress) draw() {
	e := p.last
	elapsed := time.Since(p.start)
	eta := "—"
	if e.Completed > 0 {
		remaining := p.lastCompletion/time.Duration(e.Completed)*time.Duration(e.Total) - elapsed
		eta = max(remaining, 0).Round(time.Second).String()
	}
	label := "测速中 " + displayText(e.Node)
	if s := e.Result; s != nil {
		if s.Error != "" {
			label = "× " + displayText(s.Node) + "  " + displayText(s.Error)
		} else {
			label = fmt.Sprintf("✓ %s  %.2f MiB/s", displayText(s.Node), mibPerSecond(s.Mbps))
		}
	}
	line := fmt.Sprintf("%d/%d %3d%%  已用 %s  ETA %s  %s", e.Completed, e.Total, 100*e.Completed/e.Total, elapsed.Round(time.Second), eta, label)
	if p.columns > 0 {
		const width = 12
		filled := width * e.Completed / e.Total
		line = fmt.Sprintf("[%s%s] %s", strings.Repeat("=", filled), strings.Repeat(".", width-filled), line)
		if e.Result == nil && len(line) >= p.columns {
			// A UTF-8 byte cap conservatively fits CJK and emoji as well as ASCII.
			end := p.columns - 4
			for end > 0 && !utf8.RuneStart(line[end]) {
				end--
			}
			line = line[:end] + "..."
		}
		fmt.Fprint(p.out, "\r\x1b[2K")
	}
	fmt.Fprint(p.out, line)
	if p.columns == 0 || e.Result != nil {
		fmt.Fprintln(p.out)
	}
}

func (p *benchProgress) close() {
	if p.columns == 0 {
		return
	}
	close(p.stop)
	<-p.stopped
	if p.last.Total > 0 && p.last.Result == nil {
		fmt.Fprint(p.out, "\r\x1b[2K")
	}
}
