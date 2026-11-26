package guard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
)

// SelectionResult describes the primary or its follower in execution order.
type SelectionResult struct {
	Group, Before, After, Action, Reason string
}

// Apply retains the daemon's API, including recovery candidates that have
// passed a liveness check but have no previous bandwidth measurement.
func (r *Runner) Apply(ctx context.Context, group config.Group, expectedCurrent string, ranked []model.Sample) (string, error) {
	if len(ranked) == 0 {
		return "", errors.New("no ranked candidates")
	}
	results, err := r.applySelections(ctx, BenchTarget{Group: group, Current: expectedCurrent}, ranked, false)
	if len(results) == 0 {
		return "", err
	}
	if results[0].Action == "skipped" {
		return "", errors.Join(errors.New(results[0].Reason), err)
	}
	return results[0].After, err
}

// ApplyBenchTarget applies the supplied ranking to direct physical members.
// A confirmed primary is returned even if a later follower or history write fails.
func (r *Runner) ApplyBenchTarget(ctx context.Context, target BenchTarget, ranked []model.Sample) ([]SelectionResult, error) {
	target.Group = r.benchGroup(target.Group)
	return r.applySelections(ctx, target, ranked, true)
}

func (r *Runner) applySelections(ctx context.Context, target BenchTarget, ranked []model.Sample, bench bool) (results []SelectionResult, resultErr error) {
	group := target.Group
	if group.Name == "" {
		return nil, errors.New("empty group name")
	}
	event := newDecision(target, ranked, bench, r.Cfg.Selection.NearBestPct)
	recordCandidate := recordDecisionCandidate(&event)
	var notificationOld, notificationDetail string
	var notificationResult model.Sample
	resultErr = withSelectorLock(ctx, "apply", func() error {
		primary := SelectionResult{Group: group.Name, Before: target.Current, After: target.Current, Action: "skipped"}
		if group.Name == r.Cfg.Mihomo.TestGroup {
			primary.Reason = "测速专用组仅用于扫描，不能作为切换目标"
			results = append(results, primary)
			return nil
		}
		current, err := r.Core.Proxy(ctx, group.Name)
		if err != nil {
			return fmt.Errorf("read %s: %w", model.QuoteName(group.Name), err)
		}
		primaryObserved := time.Now()
		primary.Before, primary.After = current.Now, current.Now
		if current.Type != "Selector" {
			if !bench {
				return fmt.Errorf("%s is %s, want Selector", model.QuoteName(group.Name), current.Type)
			}
			primary.Reason = nonSelectorReason(current.Type)
			results = append(results, primary)
			return nil
		}
		if bench && target.Type != "" && target.Type != "Selector" {
			primary.Reason = nonSelectorReason(target.Type) + "；测速后组类型已变化"
			results = append(results, primary)
			return nil
		}
		if (bench || target.Current != "") && current.Now != target.Current {
			return fmt.Errorf("%w: %s is now %s, expected %s", ErrManualOverride, model.QuoteName(group.Name), model.QuoteName(current.Now), model.QuoteName(target.Current))
		}
		if current.Now == r.Cfg.Mihomo.TestGroup || current.Now == "" || !contains(current.All, current.Now) {
			primary.Reason = "当前选择指向测速专用组或缺少可恢复的原成员，无法安全切换"
			results = append(results, primary)
			return nil
		}
		var eligible []model.Sample
		for _, sample := range ranked {
			unmeasured := !bench && event.Trigger == "hardFailure" && sample.Mbps == 0
			if sample.Error != "" || sample.Failures > 0 && sample.Failures >= sample.Attempts ||
				!unmeasured && (sample.Mbps <= 0 || math.IsNaN(sample.Mbps) || math.IsInf(sample.Mbps, 0)) {
				recordCandidate(sample.Node, "measurementFailed", false)
				continue
			}
			if sample.Node != "" && sample.Node != r.Cfg.Mihomo.TestGroup && contains(current.All, sample.Node) {
				eligible = append(eligible, sample)
			} else {
				recordCandidate(sample.Node, "ineligible", false)
			}
		}
		if len(eligible) == 0 {
			primary.Reason = "没有测速成功且可直接选择的候选节点"
			results = append(results, primary)
			return nil
		}

		follower := r.Cfg.Selection.Follower(group)
		var before model.Proxy
		var followerObserved time.Time
		var snapshotErr error
		var snapshotFollower func() error
		if follower != "" && follower != group.Name && follower != r.Cfg.Mihomo.TestGroup {
			snapshotFollower = func() error {
				before, snapshotErr = r.Core.Proxy(ctx, follower)
				followerObserved = time.Now()
				return nil // Follower availability is independent of the primary.
			}
		}
		chosen, detail, err := r.selectRanked(ctx, group, current, eligible, snapshotFollower, recordCandidate)
		if err != nil {
			return err
		}
		primary.After, primary.Action, primary.Reason = chosen.Node, "kept", detail
		if chosen.Node != current.Now {
			primary.Action = "switched"
		}
		results = append(results, primary)
		recordErr := r.recordSelection(ctx, primary, primaryObserved)
		if primary.Action == "kept" {
			return recordErr
		}
		if follower != "" {
			outcome, followErr := r.followSelection(ctx, follower, before, snapshotErr, group, chosen)
			results = append(results, outcome)
			followDetail := outcome.Reason
			switch outcome.Action {
			case "switched":
				followDetail = "直出组已跟随"
			case "kept":
				followDetail = "直出组已选中该节点"
			case "skipped":
				if !bench {
					r.logWarn("direct group follow skipped", "group", follower, "node", chosen.Node, "reason", outcome.Reason)
				}
			}
			if detail != "" {
				detail += "；"
			}
			detail += followDetail
			if bench {
				recordErr = errors.Join(recordErr, followErr)
			}
			recordErr = errors.Join(recordErr, r.recordSelection(ctx, outcome, followerObserved))
		}
		notificationOld, notificationResult, notificationDetail = current.Now, chosen, detail
		return recordErr
	})
	completeDecision(&event, results, resultErr)
	decisionErr := recordDecision(ctx, r.Store, event)
	eventID := ""
	if decisionErr == nil && r.Store != nil {
		eventID = event.ID
	} else if decisionErr != nil {
		r.logWarn("record selection decision failed", "group", group.Name, "action", event.Action, "error", decisionErr)
	}
	if notificationResult.Node != "" {
		r.notifySwitch(group, notificationOld, notificationResult, notificationDetail, eventID)
	}
	return results, resultErr
}

func nonSelectorReason(typ string) string {
	return fmt.Sprintf("组类型为 %s，支持测速，不支持手动切换", typ)
}

func (r *Runner) followSelection(ctx context.Context, name string, before model.Proxy, snapshotErr error, primary config.Group, chosen model.Sample) (SelectionResult, error) {
	out := SelectionResult{Group: name, Before: before.Now, Action: "skipped"}
	if name == r.Cfg.Mihomo.TestGroup || name == primary.Name {
		out.Reason = "跟随目标是测速专用组或前置组本身"
		return out, nil
	}
	if snapshotErr != nil {
		out.Reason = fmt.Sprintf("切换前无法读取跟随组 %s：%v", model.QuoteName(name), snapshotErr)
		return out, fmt.Errorf("read follower %s before primary switch: %w", model.QuoteName(name), snapshotErr)
	}
	now, err := r.Core.Proxy(ctx, name)
	if err != nil {
		out.Reason = fmt.Sprintf("无法确认跟随组当前选择：%v", err)
		return out, fmt.Errorf("re-read follower %s: %w", model.QuoteName(name), err)
	}
	out.After = now.Now
	switch {
	case now.Type != "Selector":
		out.Reason = nonSelectorReason(now.Type)
	case now.Type != before.Type || now.Now != before.Now:
		out.Reason = "跟随组已被其他操作修改，保留当前选择"
	case now.Now == r.Cfg.Mihomo.TestGroup || now.Now == "" || !contains(now.All, now.Now):
		out.Reason = "跟随组当前选择指向测速专用组或缺少可恢复的原成员，无法安全切换"
	case !contains(now.All, chosen.Node):
		out.Reason = fmt.Sprintf("跟随组没有同名的直接成员 %s", model.QuoteName(chosen.Node))
	default:
		group := r.benchGroup(config.Group{Name: name})
		_, detail, err := r.selectRanked(ctx, group, now, []model.Sample{chosen}, func() error {
			return r.verifySelection(ctx, primary, chosen.Node)
		}, nil)
		if err != nil {
			out.After = ""
			if observed, readErr := r.Core.Proxy(ctx, name); readErr == nil {
				out.After = observed.Now
			}
			out.Reason = fmt.Sprintf("跟随组切换未确认：%v", err)
			return out, fmt.Errorf("follow %s: %w", model.QuoteName(name), err)
		}
		out.After, out.Reason, out.Action = chosen.Node, detail, "switched"
		if now.Now == chosen.Node {
			out.Action = "kept"
			out.Reason = "跟随组已选中同一节点"
		}
	}
	return out, nil
}

func (r *Runner) recordSelection(ctx context.Context, result SelectionResult, observedBefore time.Time) error {
	if r.Store == nil || result.Action == "skipped" {
		return nil
	}
	switched := result.Action == "switched"
	// A verified read of the outgoing member closes its observed run, even if
	// the daemon was stopped between two manual bench invocations.
	if switched && result.Before != "" && !observedBefore.IsZero() {
		if _, err := r.Store.RecordSelection(ctx, result.Group, result.Before, observedBefore, false); err != nil {
			return fmt.Errorf("record outgoing selection history for %s: %w", model.QuoteName(result.Group), err)
		}
	}
	if _, err := r.Store.RecordSelection(ctx, result.Group, result.After, time.Now(), switched); err != nil {
		return fmt.Errorf("record selection history for %s: %w", model.QuoteName(result.Group), err)
	}
	return nil
}
