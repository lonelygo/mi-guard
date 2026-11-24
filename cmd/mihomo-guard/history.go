package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/model"
)

type DashboardSnapshot struct {
	BudgetBytes  int64          `json:"budgetBytes"`
	UsedBytes    int64          `json:"usedBytes"` // Includes pending reservations.
	PendingBytes int64          `json:"pendingBytes"`
	CycleStart   time.Time      `json:"cycleStart"`
	CycleEnd     time.Time      `json:"cycleEnd"`
	LastSweep    *time.Time     `json:"lastSweep"`
	Samples      []LedgerSample `json:"samples"`
	Groups       []LedgerGroup  `json:"groups"`
}

type LedgerSample struct {
	Time           time.Time `json:"time"`
	Node           string    `json:"node"`
	Provider       string    `json:"provider"`
	Scope          string    `json:"scope"`
	Kind           string    `json:"kind"`
	Mbps           float64   `json:"mbps"`
	LatencyMS      int       `json:"latencyMS"`
	Bytes          int64     `json:"bytes"`
	Attempts       int       `json:"attempts"`
	Failures       int       `json:"failures"`
	Error          string    `json:"error"`
	Streams        int       `json:"streams"`
	RequestedBytes int64     `json:"requestedBytes"`
	Target         string    `json:"target"`
}

type LedgerGroup struct {
	Name           string     `json:"name"`
	Chain          string     `json:"chain"`
	Configured     bool       `json:"configured"`
	Node           string     `json:"node"` // Last recorded selection, as of LastSeen.
	Since          *time.Time `json:"since"`
	LastSeen       *time.Time `json:"lastSeen"`
	GuardSelected  bool       `json:"guardSelected"`
	LongestNode    string     `json:"longestNode"`
	LongestSeconds float64    `json:"longestSeconds"`
}

func writeDashboardHistory(out io.Writer, path string) error {
	cfg, err := config.LoadGuard(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("找不到 guard 配置，可选择已有 YAML，或先运行 mihomo-guard init（%w）", os.ErrNotExist)
	}
	if err != nil {
		return errors.New("无法读取 guard 配置，请检查所选 YAML 的格式和配置项")
	}
	store := history.New(cfg.History.File, cfg.History.SamplesPerNode, cfg.Speed.BillingDay)
	past, err := store.ReadOnlySnapshot()
	if err != nil {
		return errors.New("无法读取测速账本或线路选用记录，请检查 history.file 及其 .selections.json 文件的权限和格式")
	}
	result := DashboardSnapshot{
		BudgetBytes: cfg.Speed.MonthlyBytes, UsedBytes: past.UsedBytes, PendingBytes: past.PendingBytes,
		CycleStart: past.CycleStart, CycleEnd: past.CycleStart.AddDate(0, 1, 0),
		Samples: make([]LedgerSample, 0, len(past.Samples)),
		Groups:  dashboardGroups(cfg.Groups, past.Selections),
	}
	if !past.LastSweep.IsZero() {
		result.LastSweep = &past.LastSweep
	}
	for _, sample := range past.Samples {
		result.Samples = append(result.Samples, dashboardSample(sample))
	}
	return json.NewEncoder(out).Encode(result)
}

func dashboardSample(s model.Sample) LedgerSample {
	row := LedgerSample{
		Time: s.At, Node: dashboardLabel(s.Node), Provider: dashboardLabel(s.Provider),
		Scope: dashboardLabel(s.Scope), Kind: dashboardLabel(s.Kind),
		Mbps: s.Mbps, LatencyMS: s.LatencyMS, Bytes: s.Bytes, Attempts: s.Attempts, Failures: s.Failures,
		Streams: s.Profile.Streams, RequestedBytes: s.Profile.TotalBytes, Target: s.Profile.Target,
	}
	if s.Error != "" {
		row.Error = "failed"
		// A stored provider probe status is safe to show. Raw transport errors
		// can contain the full request URL, credentials and local paths.
		if code, ok := strings.CutPrefix(s.Error, "204 probe: HTTP "); ok && len(code) == 3 {
			if status, err := strconv.Atoi(code); err == nil && status >= 100 && status <= 599 {
				row.Error = "HTTP " + code
			}
		}
	}
	return row
}

func dashboardGroups(groups []config.Group, selections map[string]history.SelectionTenure) []LedgerGroup {
	rows := make([]LedgerGroup, 0, len(groups))
	configured := make(map[string]bool, len(groups))
	for _, g := range groups {
		configured[g.Name] = true
		rows = append(rows, dashboardGroup(g.Name, g.Chain, true, selections[g.Name]))
	}
	for _, name := range slices.Sorted(maps.Keys(selections)) {
		if !configured[name] {
			rows = append(rows, dashboardGroup(name, "", false, selections[name]))
		}
	}
	return rows
}

func dashboardGroup(name, chain string, configured bool, tenure history.SelectionTenure) LedgerGroup {
	row := LedgerGroup{Name: dashboardLabel(name), Chain: dashboardLabel(chain), Configured: configured}
	if tenure.Node == "" {
		return row
	}
	row.Node, row.Since, row.LastSeen = dashboardLabel(tenure.Node), &tenure.Since, &tenure.LastSeen
	row.GuardSelected = tenure.GuardSelected
	var longest time.Duration
	var longestNode string
	for node, span := range tenure.Longest {
		if span > longest || span > 0 && span == longest && node < longestNode {
			longestNode, longest = node, span
		}
	}
	row.LongestNode, row.LongestSeconds = dashboardLabel(longestNode), longest.Seconds()
	return row
}

// Names are operator- or provider-supplied. Keep a joinable identifier even
// when a label contains a subscription URL or an absolute file path.
func dashboardLabel(label string) string {
	value := strings.TrimPrefix(label, "chain:")
	if strings.Contains(value, "://") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/") {
		digest := sha256.Sum256([]byte(label))
		return fmt.Sprintf("私有地址 · %x", digest[:8])
	}
	return label
}
