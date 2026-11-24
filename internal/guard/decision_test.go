package guard

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
)

func TestDecisionKeepsActualEvidenceAndComparableThreshold(t *testing.T) {
	target := BenchTarget{
		Group:   config.Group{Name: "warp", Chain: "chain", MinMbps: 12},
		Current: "a",
		Evidence: []model.Sample{
			{Node: "a", Provider: "provider-one", Kind: "bench", Mbps: 80, LatencyMS: 21, Attempts: 2, Profile: model.DownloadProfile{Streams: 4, TotalBytes: 16 << 20, Target: strings.Repeat("a", 64)}},
			{Node: "b", Provider: "https://private.example/sub", Kind: "bench", Error: "204 probe: HTTP 504", Failures: 1, Attempts: 1},
		},
	}
	event := newDecision(target, target.Evidence[:1], true, 20)
	if event.Source != "manual" || event.Trigger != "manualBench" || event.ThresholdMbps == nil || *event.ThresholdMbps != 64 || event.MinMbps != 12 {
		t.Fatalf("decision thresholds = %+v", event)
	}
	if len(event.Candidates) != 2 || event.Candidates[0].Streams != 4 || event.Candidates[0].RequestedBytes != 16<<20 || event.Candidates[1].Provider != "" ||
		event.Candidates[1].Stage != "probe" || event.Candidates[1].Status != "HTTP 504" {
		t.Fatalf("decision evidence = %+v", event.Candidates)
	}
	completeDecision(&event, []SelectionResult{{Group: "warp", Before: "a", After: "a", Action: "kept"}}, nil)
	if !event.Candidates[0].Selected || event.Candidates[1].Selected {
		t.Fatalf("selected evidence = %+v", event.Candidates)
	}
}

func TestDecisionFailureDoesNotPersistRawControllerError(t *testing.T) {
	event := newDecision(BenchTarget{Group: config.Group{Name: "warp"}, Current: "a"}, nil, false, 20)
	completeDecision(&event, nil, errors.New(`controller GET https://secret.example/path?token=private returned 500`))
	if event.Action != "failed" || event.After != "" || event.Reason != "候选验证未通过" {
		t.Fatalf("sanitized failure = %+v", event)
	}
	if event.At.IsZero() || len(event.ID) < 8 {
		t.Fatalf("event identity = %+v", event)
	}
}

func TestRecoveryDecisionRecordsItsActualRankingTolerance(t *testing.T) {
	event := newDecision(BenchTarget{Group: config.Group{Name: "warp"}}, []model.Sample{{Node: "a", Kind: "recovery", Mbps: 50}}, false, 20)
	if event.Trigger != "hardFailure" || event.NearBestPct != 100 || event.ThresholdMbps == nil || math.Abs(*event.ThresholdMbps) > 0.0001 {
		t.Fatalf("recovery evidence = %+v", event)
	}
	if time.Since(event.At) > time.Minute {
		t.Fatalf("decision timestamp = %v", event.At)
	}
}

func TestDecisionExplainsWhyPolicyRanksASlowerCandidateFirst(t *testing.T) {
	profile := model.DownloadProfile{Streams: 4, TotalBytes: 16 << 20, Target: strings.Repeat("a", 64)}
	sample := func(node string, speed float64, at int64, failures int) model.Sample {
		return model.Sample{At: time.Unix(at, 0), Node: node, Provider: "provider", Scope: "front", Kind: "bench",
			Profile: profile, Mbps: speed, Attempts: 10, Failures: failures, LatencyMS: 100}
	}
	fresh := []model.Sample{sample("current-fast", 100, 20, 0), sample("stable-slower", 90, 20, 0)}
	past := []model.Sample{
		sample("current-fast", 70, 1, 8), sample("current-fast", 80, 2, 8), sample("current-fast", 90, 3, 8),
		sample("stable-slower", 72, 1, 0), sample("stable-slower", 82, 2, 0), sample("stable-slower", 92, 3, 0),
	}
	ranked := policy.Rank("current-fast", fresh, past, 20)
	if ranked[0].Node != "stable-slower" {
		t.Fatalf("fixture did not exercise slower stable winner: %+v", ranked)
	}
	event := newDecision(BenchTarget{Group: config.Group{Name: "group"}, Current: "current-fast", Evidence: fresh, History: past}, ranked, true, 20)
	if event.ThresholdMbps == nil || *event.ThresholdMbps != 80 {
		t.Fatalf("threshold = %+v", event.ThresholdMbps)
	}
	fast, stable := event.Candidates[0], event.Candidates[1]
	if fast.Rank != 2 || stable.Rank != 1 || fast.PreferredBand == nil || !*fast.PreferredBand ||
		stable.PreferredBand == nil || !*stable.PreferredBand || stable.StrategyAttempts != 40 || stable.StrategyFailures != 0 ||
		stable.HistoryAttempts != 30 || stable.HistoryFailures != 0 ||
		stable.HistorySamples != 3 || stable.HistoryP25Mbps == nil || *stable.HistoryP25Mbps != 77 {
		t.Fatalf("ranking explanation fast=%+v stable=%+v", fast, stable)
	}
	completeDecision(&event, []SelectionResult{{Group: "group", Before: "stable-slower", After: "current-fast", Action: "switched"}}, nil)
	if event.Reason != "更高排名候选未通过在线验证或切换条件" {
		t.Fatalf("fallback selection explanation = %q", event.Reason)
	}
}
