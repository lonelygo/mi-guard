package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/model"
)

type benchSize struct{ columns, rows int }

func terminalSize(f *os.File) benchSize {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = f
	b, err := cmd.Output()
	var size benchSize
	if err == nil {
		fmt.Sscan(string(b), &size.rows, &size.columns)
	}
	if size.columns < 1 || size.rows < 1 {
		return benchSize{}
	}
	return size
}

func sharedBenchTerminal(stdout, stderr *os.File) benchSize {
	a, aErr := stdout.Stat()
	b, bErr := stderr.Stat()
	if aErr != nil || bErr != nil || os.Getenv("TERM") == "dumb" ||
		a.Mode()&os.ModeCharDevice == 0 || !os.SameFile(a, b) {
		return benchSize{}
	}
	return terminalSize(stderr)
}

type benchProgress struct {
	out            io.Writer
	size           benchSize
	style          benchStyle
	heading        []string
	start          time.Time
	mu             sync.Mutex
	last           guard.ScanProgress
	lastCompletion time.Duration
	ranked         []model.Sample
	stop, stopped  chan struct{}
	resize         chan os.Signal
}

func newBenchProgress(out io.Writer, size benchSize) *benchProgress {
	p := &benchProgress{out: out, size: size, style: os.Getenv("NO_COLOR") == "", start: time.Now()}
	const loading = "正在读取测速候选…"
	if size.columns > 0 {
		p.stop, p.stopped = make(chan struct{}), make(chan struct{})
		if _, ok := out.(*os.File); ok {
			p.resize = make(chan os.Signal, 1)
			signal.Notify(p.resize, syscall.SIGWINCH)
		}
		fmt.Fprint(out, "\x1b[?1049h\x1b[?25l\x1b[H", fitCells(loading, size.columns-1), "\x1b[J")
		go p.tick()
	} else {
		fmt.Fprintln(out, loading)
	}
	return p
}

func (p *benchProgress) describe(groups, host string, streams int, options benchOptions, timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := byteCount(options.bytes)
	perStream := byteCount(options.bytesPerStream)
	p.start = time.Now()
	if p.stop == nil {
		capacity := fmt.Sprintf("每节点共 %s，按流均分", total)
		if options.bytesPerStream > 0 {
			capacity = fmt.Sprintf("每流上限 %s，每节点共 %s", perStream, total)
		}
		fmt.Fprintf(p.out, "下载目标 %s · 每节点 %d 流 · %s · 超时 %s\n", host, streams, capacity, timeout)
		return
	}
	capacity := fmt.Sprintf("%d 流均分 %s/节点", streams, total)
	if options.bytesPerStream > 0 {
		capacity = fmt.Sprintf("%d 流 × %s · %s/节点", streams, perStream, total)
	}
	capacity = strings.NewReplacer(".00 ", " ", ".0 ", " ").Replace(capacity)
	p.heading = []string{groups, fmt.Sprintf("%s · 超时 %s · %s", capacity, timeout, host)}
}

func (p *benchProgress) tick() {
	defer close(p.stopped)
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		var resized benchSize
		select {
		case <-p.stop:
			return
		case <-p.resize:
			resized = terminalSize(p.out.(*os.File))
		case <-timer.C:
		}
		p.mu.Lock()
		if resized.columns > 0 {
			p.size = resized
		}
		if p.last.Total > 0 {
			p.draw()
		}
		p.mu.Unlock()
	}
}

func (p *benchProgress) update(e guard.ScanProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = e
	if e.Result != nil {
		p.lastCompletion = time.Since(p.start)
		if p.size.columns > 0 {
			p.ranked = append(p.ranked, *e.Result)
			rankBenchResults(p.ranked)
		}
	}
	if p.size.columns > 0 {
		p.draw()
		return
	}
	label := "测速中 " + displayText(e.Node)
	if s := e.Result; s != nil {
		if s.Error != "" {
			label = "× " + displayText(s.Node) + "  " + displayText(s.Error)
		} else {
			label = fmt.Sprintf("✓ %s  %.2f MiB/s", displayText(s.Node), mibPerSecond(s.Mbps))
		}
	}
	elapsed := time.Since(p.start)
	eta := "—"
	if e.Completed > 0 {
		eta = p.remaining(elapsed).Round(time.Second).String()
	}
	fmt.Fprintf(p.out, "%d/%d %3d%%  已用 %s  ETA %s  %s\n", e.Completed, e.Total,
		100*e.Completed/e.Total, elapsed.Round(time.Second), eta, label)
}

func (p *benchProgress) remaining(elapsed time.Duration) time.Duration {
	return max(0, p.lastCompletion/time.Duration(p.last.Completed)*time.Duration(p.last.Total)-elapsed)
}

func benchClock(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func (p *benchProgress) status(width int, label string, elapsed time.Duration) string {
	e := p.last
	left := fmt.Sprintf("%s %d/%d", label, e.Completed, e.Total)
	percent := fmt.Sprintf("%3d%%", 100*e.Completed/e.Total)
	right := ""
	if width >= 32 {
		right = "  已用 " + benchClock(elapsed)
	}
	if width >= 48 && e.Completed < e.Total && label != "已停止" {
		eta := "—"
		if e.Completed > 0 {
			eta = benchClock(p.remaining(elapsed))
		}
		right += "  剩余 " + eta
	}
	barWidth := min(40, width-cellWidth(left+percent+right)-4)
	if barWidth < 4 {
		return fitCells(left+"  "+percent+right, width)
	}
	filled := barWidth * e.Completed / e.Total
	bar := p.style.paint("35", strings.Repeat("█", filled)) + p.style.paint("2", strings.Repeat("░", barWidth-filled))
	return left + "  " + bar + "  " + p.style.paint("1", percent) + right
}

func (p *benchProgress) frame(elapsed time.Duration) []string {
	e := p.last
	table := newBenchTable(p.size.columns-1, e.Total, p.style)
	height := p.size.rows
	var lines []string
	if height >= 9 {
		lines = append(lines, p.headingLines(table.width)...)
		if len(lines) > 0 {
			lines = append(lines, "")
		}
	}
	lines = append(lines, p.status(table.width, "已完成", elapsed))
	nodeLabel := "正在测速  "
	if e.Result != nil {
		nodeLabel = "已测节点  "
	}
	lines = append(lines, fitCells(nodeLabel+displayText(e.Node), table.width))
	if height >= 8 {
		lines = append(lines, "")
	}
	lines = append(lines, table.header(), table.divider())
	available := max(0, height-len(lines))
	visible := min(available, len(p.ranked))
	if visible < len(p.ranked) && available > 1 {
		visible-- // reserve a line for the number outside the live viewport
	}
	for i, sample := range p.ranked[:visible] {
		lines = append(lines, table.sample(i+1, sample))
	}
	if n := len(p.ranked) - visible; n > 0 && len(lines) < height {
		lines = append(lines, p.style.paint("2", fitCells(fmt.Sprintf("另有 %d 个结果", n), table.width)))
	}
	return lines[:min(len(lines), height)]
}

func (p *benchProgress) headingLines(width int) []string {
	var lines []string
	for i, s := range p.heading {
		style := "2"
		if i == 0 {
			style = "1"
		}
		if width > 0 {
			s = fitCells(s, width)
		}
		lines = append(lines, p.style.paint(style, s))
	}
	return lines
}

func (p *benchProgress) draw() {
	// Home is relative to the alternate screen. Clear the rest of each line
	// before advancing so shorter frames and terminal resizes leave no debris.
	frame := p.frame(time.Since(p.start))
	fmt.Fprint(p.out, "\x1b[H", strings.Join(frame, "\x1b[K\r\n"), "\x1b[J")
}

func (p *benchProgress) close() {
	if p.stop == nil {
		return
	}
	if p.resize != nil {
		signal.Stop(p.resize)
	}
	close(p.stop)
	<-p.stopped
	p.stop = nil
	fmt.Fprint(p.out, "\x1b[?25h\x1b[?1049l")
}

func (p *benchProgress) printFinal(out io.Writer, results []model.Sample, scanErr error) {
	if p.size.columns == 0 {
		printResults(out, results)
		return
	}
	table := newBenchTable(p.size.columns-1, p.last.Total, p.style)
	for _, line := range p.headingLines(0) {
		fmt.Fprintln(out, line)
	}
	if p.last.Total > 0 {
		label := "已完成"
		if scanErr != nil {
			label = "已停止"
		}
		fmt.Fprintln(out, p.status(table.width, label, time.Since(p.start)))
	}
	if len(p.ranked) > 0 {
		fmt.Fprintln(out)
		table.print(out, p.ranked)
		fmt.Fprintln(out)
	}
}
