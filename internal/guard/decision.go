package guard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
)

func newDecision(target BenchTarget, ranked []model.Sample, bench bool, nearBestPct float64) history.Decision {
	at := time.Now().UTC()
	idBytes := make([]byte, 16)
	id := ""
	if _, err := rand.Read(idBytes); err == nil {
		id = hex.EncodeToString(idBytes)
	} else {
		id = fmt.Sprintf("%x-%x", os.Getpid(), at.UnixNano())
	}
	triggerSamples := ranked
	if len(triggerSamples) == 0 {
		triggerSamples = target.Evidence
	}
	trigger := target.DecisionTrigger
	if trigger == "" {
		trigger = decisionTrigger(triggerSamples, bench)
	}
	if trigger == "hardFailure" {
		// Recovery deliberately ranks every live alternative. It does not apply
		// the normal near-best bandwidth tier.
		nearBestPct = 100
	}
	event := history.Decision{
		ID:          id,
		At:          at,
		Source:      "daemon",
		Group:       safeDecisionLabel(target.Group.Name),
		Chain:       safeDecisionLabel(target.Group.Chain),
		Before:      safeDecisionLabel(target.Current),
		After:       safeDecisionLabel(target.Current),
		Action:      "failed",
		Trigger:     trigger,
		NearBestPct: nearBestPct,
		MinMbps:     target.Group.MinMbps,
	}
	if trigger != "hardFailure" && target.LatencyGate.Factor > 0 {
		event.LatencyFactor = target.LatencyGate.Factor
		event.LatencySamples = target.LatencyGate.Samples
		if target.LatencyGate.Samples > 0 && target.LatencyGate.MeanMS >= 0 &&
			!math.IsNaN(target.LatencyGate.MeanMS) && !math.IsInf(target.LatencyGate.MeanMS, 0) {
			mean := target.LatencyGate.MeanMS
			event.LatencyMeanMS = &mean
		}
		if threshold, known := target.LatencyGate.Threshold(); known {
			event.LatencyThresholdMS = &threshold
		}
	}
	if bench {
		event.Source = "manual"
	}
	evidence := target.Evidence
	if len(evidence) == 0 {
		evidence = ranked
	}
	event.Candidates = make([]history.DecisionCandidate, 0, len(evidence))
	for _, sample := range evidence {
		mbps := sample.Mbps
		if math.IsNaN(mbps) || math.IsInf(mbps, 0) || mbps < 0 {
			mbps = 0
		}
		stage, status := decisionFailure(sample)
		candidate := history.DecisionCandidate{
			Node:           safeDecisionLabel(sample.Node),
			Provider:       safeDecisionMetadata(sample.Provider),
			Kind:           sample.Kind,
			Target:         safeDecisionTarget(sample.Profile.Target),
			Mbps:           mbps,
			LatencyMS:      sample.LatencyMS,
			Attempts:       sample.Attempts,
			Failures:       sample.Failures,
			Streams:        sample.Profile.Streams,
			RequestedBytes: sample.Profile.TotalBytes,
			Bytes:          sample.Bytes,
			Stage:          stage,
			Status:         status,
		}
		if trigger != "hardFailure" {
			candidate.Validation = target.LatencyGate.Exclusion(sample)
		}
		if !sample.At.IsZero() {
			observedAt := sample.At.UTC()
			candidate.ObservedAt = &observedAt
		}
		event.Candidates = append(event.Candidates, candidate)
	}
	if trigger != "hardFailure" && len(ranked) > 0 {
		if threshold, known := policy.PreferredThreshold(target.LatencyGate.Filter(evidence), nearBestPct); known {
			event.ThresholdMbps = &threshold
		}
	}
	annotateDecisionRanking(&event, evidence, ranked, target.History)
	return event
}

func recordDecisionCandidate(event *history.Decision) func(string, string, bool) {
	order := 0
	return func(node, result string, online bool) {
		node = safeDecisionLabel(node)
		index := decisionCandidateIndex(event.Candidates, node, online)
		if index < 0 {
			return
		}
		candidate := &event.Candidates[index]
		if online {
			// Each terminal online result advances the chronology. A permissive
			// geo fallback revisits the first live candidate after every strict
			// geo attempt, so its final success must receive the later order.
			order++
			candidate.ValidationOrder = order
		}
		candidate.Validation = result
	}
}

func decisionCandidateIndex(candidates []history.DecisionCandidate, node string, online bool) int {
	if online {
		// A second result for an already attempted node is the permissive geo
		// fallback. Keep it on that same evidence row instead of borrowing a
		// duplicate row that may have been filtered before online validation.
		for index, candidate := range candidates {
			if candidate.Node == node && candidate.ValidationOrder > 0 {
				return index
			}
		}
		// Prefer the ranked, still-unannotated sample when the evidence contains
		// more than one sample for the same node.
		for index, candidate := range candidates {
			if candidate.Node == node && candidate.Rank > 0 && candidate.Validation == "" {
				return index
			}
		}
	}
	for index, candidate := range candidates {
		if candidate.Node == node && candidate.Validation == "" {
			return index
		}
	}
	return -1
}

func annotateDecisionRanking(event *history.Decision, evidence, ranked, past []model.Sample) {
	used := make([]bool, len(evidence))
	for rank, sample := range ranked {
		index := -1
		for candidateIndex, candidate := range evidence {
			if !used[candidateIndex] && candidate == sample {
				index = candidateIndex
				break
			}
		}
		if index < 0 {
			for candidateIndex, candidate := range evidence {
				if !used[candidateIndex] && candidate.Node == sample.Node && candidate.Provider == sample.Provider &&
					candidate.Scope == sample.Scope && candidate.Kind == sample.Kind && candidate.Profile == sample.Profile {
					index = candidateIndex
					break
				}
			}
		}
		if index < 0 {
			continue
		}
		used[index] = true
		candidate := &event.Candidates[index]
		candidate.Rank = rank + 1
		if event.Trigger != "hardFailure" && event.ThresholdMbps != nil && policy.HasBandwidth(sample) {
			preferred := sample.Mbps >= *event.ThresholdMbps
			candidate.PreferredBand = &preferred
		}
		explanation := policy.ExplainHistory(sample, past)
		candidate.StrategyAttempts = explanation.Attempts
		candidate.StrategyFailures = explanation.Failures
		candidate.HistoryAttempts = explanation.PastAttempts
		candidate.HistoryFailures = explanation.PastFailures
		candidate.HistorySamples = explanation.BandwidthCount
		if explanation.P25Known {
			p25 := explanation.P25Mbps
			candidate.HistoryP25Mbps = &p25
		}
	}
}

func decisionFailure(sample model.Sample) (stage, status string) {
	if sample.Error == "" && sample.Failures == 0 {
		return "", ""
	}
	if code, ok := strings.CutPrefix(sample.Error, "204 probe: HTTP "); ok && len(code) == 3 {
		allDigits := true
		for _, character := range code {
			allDigits = allDigits && character >= '0' && character <= '9'
		}
		if allDigits {
			return "probe", "HTTP " + code
		}
	}
	if sample.Error == "" {
		return "probe", "failed"
	}
	if sample.Bytes > 0 {
		return "download", "interrupted"
	}
	return "download", "failed"
}

func decisionTrigger(ranked []model.Sample, bench bool) string {
	if bench {
		return "manualBench"
	}
	for _, sample := range ranked {
		switch sample.Kind {
		case "recovery":
			return "hardFailure"
		case "confirm":
			return "slowConfirmation"
		case "sweep":
			return "scheduledSweep"
		case "quick":
			return "activeCheck"
		}
	}
	return "daemon"
}

func safeDecisionMetadata(value string) string {
	if value == "" || len(value) > 256 || strings.Contains(value, "://") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/") {
		return ""
	}
	return value
}

func safeDecisionLabel(value string) string {
	if value == "" {
		return ""
	}
	if strings.Contains(value, "://") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/") {
		digest := sha256.Sum256([]byte(value))
		return fmt.Sprintf("私有地址 · %x", digest[:8])
	}
	return value
}

func safeDecisionTarget(value string) string {
	if len(value) != 64 {
		return ""
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return ""
		}
	}
	return value
}

func completeDecision(event *history.Decision, results []SelectionResult, applyErr error) {
	if len(results) > 0 {
		primary := results[0]
		event.Before, event.After, event.Action = safeDecisionLabel(primary.Before), safeDecisionLabel(primary.After), primary.Action
		event.Reason = safeDecisionReason(primary.Action, primary.Reason)
		for _, result := range results[1:] {
			event.Followers = append(event.Followers, history.DecisionFollower{
				Group: safeDecisionLabel(result.Group), Before: safeDecisionLabel(result.Before), After: safeDecisionLabel(result.After),
				Action: result.Action, Reason: safeDecisionReason(result.Action, result.Reason),
			})
		}
	}
	if applyErr != nil {
		if event.Action != "switched" && event.Action != "kept" {
			event.Action = "failed"
			event.After = ""
			event.Reason = safeDecisionError(applyErr)
		}
	}
	hasOnlineEvidence := false
	for index := range event.Candidates {
		candidate := &event.Candidates[index]
		candidate.Selected = false
		hasOnlineEvidence = hasOnlineEvidence || candidate.ValidationOrder > 0
	}
	if hasOnlineEvidence {
		for index := range event.Candidates {
			candidate := &event.Candidates[index]
			if candidate.Rank > 0 && candidate.Validation == "" {
				candidate.Validation = "notReached"
			}
		}
	}
	selectedIndex := selectedDecisionCandidate(event.Candidates, event.After, event.Action)
	if selectedIndex >= 0 {
		event.Candidates[selectedIndex].Selected = true
		selected := event.Candidates[selectedIndex]
		if candidateValidationSucceeded(selected.Validation) && selected.ValidationOrder > 1 && event.Reason == "" {
			event.Reason = fmt.Sprintf(
				"按排序完成 %d 次在线验证，前 %d 次未通过；排序 #%d 通过并被采用",
				selected.ValidationOrder, selected.ValidationOrder-1, selected.Rank,
			)
		}
	}
}

func selectedDecisionCandidate(candidates []history.DecisionCandidate, node, action string) int {
	if node == "" || action != "switched" && action != "kept" {
		return -1
	}
	best, bestOrder := -1, -1
	for index, candidate := range candidates {
		if candidate.Node == node && candidateValidationSucceeded(candidate.Validation) && candidate.ValidationOrder > bestOrder {
			best, bestOrder = index, candidate.ValidationOrder
		}
	}
	if best >= 0 {
		return best
	}
	best, bestRank := -1, int(^uint(0)>>1)
	for index, candidate := range candidates {
		if candidate.Node == node && candidate.Rank > 0 && candidate.Rank < bestRank {
			best, bestRank = index, candidate.Rank
		}
	}
	if best >= 0 {
		return best
	}
	for index, candidate := range candidates {
		if candidate.Node == node && candidate.Validation != "latencyAboveLimit" && candidate.Validation != "latencyUnknown" {
			return index
		}
	}
	return -1
}

func candidateValidationSucceeded(validation string) bool {
	switch validation {
	case "passed", "fallbackRegionMismatch", "fallbackRegionProbeFailed", "fallbackRegionUnknown":
		return true
	default:
		return false
	}
}

func safeDecisionReason(action, reason string) string {
	if reason == "" {
		return ""
	}
	safePrefixes := []string{
		"测速专用组仅用于扫描", "组类型为 ", "当前选择指向测速专用组", "没有测速成功且可直接选择的候选节点",
		"Google 地区校验未通过", "出口地区不符，按宽松策略", "地区探测失败，按宽松策略", "地区结果未确认，按宽松策略",
		"跟随目标是测速专用组", "跟随组已被其他操作修改", "跟随组当前选择指向测速专用组",
		"跟随组没有同名的直接成员", "跟随组已选中同一节点",
		"延迟筛选后",
	}
	for _, prefix := range safePrefixes {
		if strings.HasPrefix(reason, prefix) {
			if prefix == "跟随组没有同名的直接成员" {
				return prefix
			}
			return reason
		}
	}
	if action == "skipped" {
		return "未满足安全切换条件"
	}
	return ""
}

func safeDecisionError(err error) string {
	switch {
	case errors.Is(err, ErrManualOverride):
		return "选择已被其他操作修改"
	case errors.Is(err, context.Canceled):
		return "操作已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "候选验证超时"
	default:
		return "候选验证未通过"
	}
}

func recordDecision(ctx context.Context, store *history.Store, event history.Decision) error {
	if store == nil {
		return nil
	}
	recordContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return store.RecordDecision(recordContext, event)
}

func (r *Runner) recordDecisionOutcome(ctx context.Context, group config.Group, current, action, reason string, evidence []model.Sample, gate policy.LatencyGate) {
	event := newDecision(BenchTarget{Group: group, Current: current, Evidence: evidence, LatencyGate: gate}, nil, false, r.Cfg.Selection.NearBestPct)
	event.After, event.Action, event.Reason = safeDecisionLabel(current), action, reason
	for index := range event.Candidates {
		candidate := &event.Candidates[index]
		candidate.Selected = action == "kept" && candidate.Node == event.After &&
			candidate.Validation != policy.LatencyAboveLimit && candidate.Validation != policy.LatencyUnknown
	}
	if err := recordDecision(ctx, r.Store, event); err != nil {
		r.logWarn("record selection decision failed", "group", group.Name, "action", action, "error", err)
	}
}
