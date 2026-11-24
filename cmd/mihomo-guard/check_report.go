package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"github.com/lonelygo/mi-guard/internal/model"
)

func readBandwidth(ctx context.Context, r *guard.Runner) (map[string]model.Sample, error) {
	snapshot, err := r.Store.Snapshot(ctx)
	if err != nil || len(snapshot.Samples) == 0 {
		return nil, err
	}
	inventory, err := r.Core.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	return latestBandwidth(snapshot.Samples, inventory, time.Now()), nil
}

func latestBandwidth(samples []model.Sample, inventory map[string]model.Proxy, now time.Time) map[string]model.Sample {
	result := make(map[string]model.Sample)
	for _, s := range samples {
		node, ok := inventory[s.Node]
		if !ok || node.Name != s.Node || node.Provider != s.Provider || node.All != nil || node.Type == "Ambiguous" {
			continue
		}
		scope := "front"
		if node.Dialer != "" {
			scope = "chain:" + node.Name
		}
		if s.Scope != scope || s.Error != "" || s.Failures != 0 || !(s.Mbps > 0) || math.IsInf(s.Mbps, 0) ||
			s.At.IsZero() || s.At.After(now) || s.At.Before(result[s.Node].At) {
			continue
		}
		switch s.Kind {
		case "bench", "sweep", "confirm", "quick":
			result[s.Node] = s
		}
	}
	return result
}

func durationLabel(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh%dm", d/(24*time.Hour), d/time.Hour%24, d/time.Minute%60)
	}
	return d.Truncate(time.Second).String()
}

func selectionSpan(current string, tenure history.SelectionTenure, now time.Time) string {
	if current == "" || tenure.Node != current || tenure.Since.IsZero() || tenure.LastSeen.After(now) {
		return "未知"
	}
	if tenure.LastSeen.Equal(tenure.Since) && !tenure.GuardSelected {
		return "首次观察，尚无更早记录"
	}
	origin := "自首次观察"
	if tenure.GuardSelected {
		origin = "自 guard 切换"
	}
	return durationLabel(tenure.LastSeen.Sub(tenure.Since)) + "（" + origin + "）"
}

func reportLiveGroup(out io.Writer, group config.Group, current string, nodes []string, probes map[string]memberProbe, sample model.Sample, tenure history.SelectionTenure, now time.Time, maxAge time.Duration) error {
	target := "选择组 " + displayText(group.Name)
	if group.Chain != "" {
		target = displayText(group.Chain) + " → 前置组 " + displayText(group.Name)
	}
	healthy, unknown := 0, 0
	for _, n := range nodes {
		p, present := probes[n]
		if present && p.err == nil {
			healthy++
		} else if _, offline := mihomo.ProbeStatus(p.err); !offline {
			unknown++
		}
	}
	uncertain := ""
	if unknown > 0 {
		uncertain = fmt.Sprintf("，%d 个未能确认", unknown)
	}
	delay := "未知"
	if p, ok := probes[current]; ok {
		delay = p.label()
	}
	bandwidth := "暂无测速记录"
	if sample.Mbps > 0 {
		age := now.Sub(sample.At)
		freshness := "前"
		if age > maxAge {
			freshness = "前，已过期"
		}
		bandwidth = fmt.Sprintf("%.2f Mbps / %.2f MiB/s（%s，%s%s）", sample.Mbps, mibPerSecond(sample.Mbps), sample.Kind, durationLabel(age), freshness)
	}
	longestNode, longest := "", time.Duration(0)
	for node, span := range tenure.Longest {
		if span > longest || span > 0 && span == longest && node < longestNode {
			longestNode, longest = node, span
		}
	}
	record := ""
	if longestNode != "" {
		record = fmt.Sprintf("  ·  最长选用记录 %s %s", displayText(longestNode), durationLabel(longest))
	}
	name := displayText(current)
	if name == "" {
		name = "未知"
	}
	_, err := fmt.Fprintf(out, "%s  ·  本次探测可用 %d/%d%s\n  当前 %s  ·  HTTP 延迟 %s\n  最近带宽 %s\n  已选用 %s%s\n",
		target, healthy, len(nodes), uncertain, name, delay, bandwidth, selectionSpan(current, tenure, now), record)
	return err
}
