package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/lonelygo/mi-guard/internal/model"
)

type benchStyle bool

func (color benchStyle) paint(code, s string) string {
	if !color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

const (
	rankCol = iota
	nodeCol
	typeCol
	latencyCol
	failCol
	bytesCol
	mbpsCol
	speedCol
	columnCount
)

type benchTable struct {
	width  int
	fields [columnCount]int
	style  benchStyle
}

// Give the name the remaining space. The same widths apply to the header
// and every update, including before the first sample arrives.
func newBenchTable(width, total int, style benchStyle) benchTable {
	t := benchTable{width: max(1, min(width, 112)), style: style}
	t.fields[speedCol] = min(t.width, 7)
	if t.width >= 24 {
		t.fields[rankCol] = max(2, len(strconv.Itoa(total)))
	}
	if t.width >= 40 {
		t.fields[latencyCol] = 9
	}
	if t.width >= 58 {
		t.fields[typeCol] = 11
	}
	if t.width >= 74 {
		t.fields[mbpsCol] = 9
	}
	if t.width >= 88 {
		t.fields[failCol] = 9
	}
	if t.width >= 100 {
		t.fields[bytesCol] = 10
	}
	if t.width >= 46 {
		t.fields[speedCol] = 12
	}
	available := t.width
	for _, w := range t.fields {
		if w > 0 {
			available -= w + 2
		}
	}
	t.fields[nodeCol] = max(0, available)
	return t
}

func (t benchTable) header() string {
	speed := "MiB/s ↓"
	if t.fields[speedCol] >= 12 {
		speed = "下载 MiB/s ↓"
	}
	cells := [columnCount]string{"#", "节点名称", "类型", "HTTP 延迟", "失败/尝试", "已读取", "Mbps", speed}
	return t.style.paint("1", t.line(cells, false, false))
}

func (t benchTable) divider() string {
	return t.style.paint("2", strings.Repeat("─", t.width))
}

func (t benchTable) sample(rank int, s model.Sample) string {
	mbps, speed, latency := "—", "—", "—"
	if s.Error != "" {
		speed = "失败"
	} else if s.Mbps > 0 {
		mbps = fmt.Sprintf("%.3f", s.Mbps)
		speed = fmt.Sprintf("%.2f", mibPerSecond(s.Mbps))
	}
	if ms, ok := s.ProbeLatency(); ok {
		latency = fmt.Sprintf("%d ms", ms)
	}
	cells := [columnCount]string{
		strconv.Itoa(rank), displayText(s.Node), displayText(s.ProxyType), latency,
		fmt.Sprintf("%d/%d", s.Failures, s.Attempts), byteCount(s.Bytes), mbps, speed,
	}
	return t.line(cells, rank == 1 && s.Mbps > 0 && s.Error == "", s.Error != "")
}

func (t benchTable) line(cells [columnCount]string, first, failed bool) string {
	var fields []string
	for i, w := range t.fields {
		if w == 0 {
			continue
		}
		value := fitCells(cells[i], w)
		padding := strings.Repeat(" ", w-cellWidth(value))
		switch {
		case failed && i == speedCol:
			value = t.style.paint("33", value)
		case first && (i == rankCol || i == speedCol):
			value = t.style.paint("1;36", value)
		case first && i == nodeCol:
			value = t.style.paint("1", value)
		}
		if i == nodeCol || i == typeCol {
			fields = append(fields, value+padding)
		} else {
			fields = append(fields, padding+value)
		}
	}
	return strings.Join(fields, "  ")
}

// Keep long names and failure reasons available in the final transcript
// without allowing one long value to shift the table columns.
func (t benchTable) print(out io.Writer, rows []model.Sample) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out, t.header())
	fmt.Fprintln(out, t.divider())
	for i, s := range rows {
		fmt.Fprintln(out, t.sample(i+1, s))
	}
	for i, s := range rows {
		name := displayText(s.Node)
		var detail []string
		if cellWidth(name) > t.fields[nodeCol] {
			detail = append(detail, name)
		}
		if s.Error != "" {
			detail = append(detail, displayText(s.Error))
		}
		if len(detail) > 0 {
			fmt.Fprintf(out, "  #%d  %s\n", i+1, strings.Join(detail, " · "))
		}
	}
}
