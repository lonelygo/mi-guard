package guard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

var ErrManualOverride = errors.New("proxy selection changed outside mihomo-guard")

// selectRanked runs under the apply lock. An unsuccessful trial rolls back
// only while the selector still holds the last value written by this call.
func (r *Runner) selectRanked(ctx context.Context, group config.Group, current model.Proxy, ranked []model.Sample, beforeFirstWrite func() error) (chosen model.Sample, reason string, err error) {
	original := current.Now
	owned := original
	defer func() {
		if err != nil && owned != original {
			err = errors.Join(err, r.rollbackSelection(group, original, owned))
		}
	}()

	var failures []string
	// A false result with no error means the controller rejected this
	// candidate and a fresh read confirmed our previous selection.
	move := func(node string) (bool, error) {
		if beforeFirstWrite != nil {
			if err := beforeFirstWrite(); err != nil {
				return false, err
			}
			beforeFirstWrite = nil
		}
		if err := r.validateLiveDialer(ctx, group); err != nil {
			return false, err
		}
		connections, snapshotErr := r.selectionConnections(ctx, group, owned)
		if snapshotErr != nil {
			r.logWarn("snapshot chain connections failed", "group", group.Name, "node", owned, "error", snapshotErr)
		}
		latest, err := r.Core.Proxy(ctx, group.Name)
		if err != nil {
			return false, fmt.Errorf("re-read %s: %w", model.QuoteName(group.Name), err)
		}
		if latest.Type != "Selector" || latest.Now != owned || !contains(latest.All, node) {
			return false, fmt.Errorf("%w: %s is now %s (%s), expected %s with member %s", ErrManualOverride, model.QuoteName(group.Name), model.QuoteName(latest.Now), latest.Type, model.QuoteName(owned), model.QuoteName(node))
		}
		if selectErr := r.Core.Select(ctx, group.Name, node); selectErr != nil {
			observed, readErr := r.readSelectionAfterUncertainWrite(group.Name)
			if readErr != nil {
				return false, errors.Join(selectErr, readErr)
			}
			if observed == node {
				owned = node
				r.closeConnectionsAfterUncertainWrite(group.Name, connections)
				return false, fmt.Errorf("select %s response failed after controller applied it: %w", model.QuoteName(node), selectErr)
			}
			if observed != owned {
				return false, fmt.Errorf("%w: %s changed to %s", ErrManualOverride, model.QuoteName(group.Name), model.QuoteName(observed))
			}
			failures = append(failures, fmt.Sprintf("%s select: %v", node, selectErr))
			return false, nil
		}
		owned = node
		if err := r.Core.CloseConnections(ctx, connections); err != nil {
			r.logWarn("connection cleanup failed", "group", group.Name, "node", node, "error", err)
		}
		return true, nil
	}

	var exclude *regexp.Regexp
	if group.Exclude != "" {
		exclude, err = regexp.Compile(group.Exclude)
		if err != nil {
			return chosen, "", fmt.Errorf("%s exclude_regex: %w", model.QuoteName(group.Name), err)
		}
	}
	seen := make(map[string]bool, len(ranked))
	var firstLive model.Sample
	for _, result := range ranked {
		node := result.Node
		if seen[node] {
			continue
		}
		seen[node] = true
		if !contains(current.All, node) || node == r.Cfg.Mihomo.TestGroup || exclude != nil && exclude.MatchString(node) {
			failures = append(failures, fmt.Sprintf("%s is not an eligible member of %s", node, group.Name))
			continue
		}
		if group.MinMbps > 0 && result.Mbps > 0 && result.Mbps < group.MinMbps {
			failures = append(failures, fmt.Sprintf("%s below %.1f Mbps", node, group.MinMbps))
			continue
		}
		candidate, readErr := r.Core.Proxy(ctx, node)
		if readErr != nil {
			failures = append(failures, fmt.Sprintf("%s read: %v", node, readErr))
			continue
		}
		if !physical(candidate) || candidate.Dialer != "" && candidate.Dialer == r.Cfg.Mihomo.TestGroup {
			failures = append(failures, fmt.Sprintf("%s is not a physical proxy eligible for selection", node))
			continue
		}
		if node != original {
			if _, delayErr := r.Core.Delay(ctx, node, r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout); delayErr != nil {
				failures = append(failures, fmt.Sprintf("%s unavailable", node))
				continue
			}
		}
		if owned != node {
			applied, err := move(node)
			if err != nil {
				return chosen, "", err
			}
			if !applied {
				if node == original {
					return chosen, "", fmt.Errorf("restore retained original: %s", strings.Join(failures, "; "))
				}
				continue
			}
		}
		if _, pathErr := r.Core.Delay(ctx, group.ProbeTarget(), r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout); pathErr != nil {
			failures = append(failures, fmt.Sprintf("%s live path: %v", node, pathErr))
			continue
		}
		if firstLive.Node == "" {
			firstLive = result
		}
		if group.GeoProxy != "" {
			geo, geoErr := r.checkGeo(ctx, group)
			if errors.Is(geoErr, probe.ErrRoute) {
				return chosen, "", fmt.Errorf("prove live geo route for %s: %w", model.QuoteName(group.Name), geoErr)
			}
			if !geo.OK {
				failures = append(failures, fmt.Sprintf("%s geo: %s", node, geoFailure(geo, geoErr)))
				continue
			}
			if geoErr != nil {
				r.logWarn("exit telemetry failed after Google region passed", "group", group.Name, "node", node, "error", geoErr)
			}
		}
		return result, "", r.verifySelection(ctx, group, node)
	}

	if !group.StrictGeo && firstLive.Node != "" {
		if owned != firstLive.Node {
			applied, err := move(firstLive.Node)
			if err != nil {
				return chosen, "", err
			}
			if !applied {
				return chosen, "", fmt.Errorf("select live fallback: %s", strings.Join(failures, "; "))
			}
			if _, err := r.Core.Delay(ctx, group.ProbeTarget(), r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout); err != nil {
				return chosen, "", fmt.Errorf("verify fallback route for %s: %w", model.QuoteName(group.Name), err)
			}
		}
		return firstLive, "Google 地区校验未通过，采用首个链路可用候选", r.verifySelection(ctx, group, firstLive.Node)
	}
	return chosen, "", fmt.Errorf("no candidate passed validation: %s", strings.Join(failures, "; "))
}

func (r *Runner) verifySelection(ctx context.Context, group config.Group, node string) error {
	if err := r.validateLiveDialer(ctx, group); err != nil {
		return err
	}
	verified, err := r.Core.Proxy(ctx, group.Name)
	if err != nil {
		return fmt.Errorf("verify selection for %s: %w", model.QuoteName(group.Name), err)
	}
	if verified.Type != "Selector" || verified.Now != node || !contains(verified.All, node) {
		return fmt.Errorf("%w: %s is now %s (%s), expected %s", ErrManualOverride, model.QuoteName(group.Name), model.QuoteName(verified.Now), verified.Type, model.QuoteName(node))
	}
	return nil
}

func (r *Runner) rollbackSelection(group config.Group, original, owned string) error {
	rollbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.validateLiveDialer(rollbackContext, group); err != nil {
		return err
	}
	connections, snapshotErr := r.selectionConnections(rollbackContext, group, owned)
	if snapshotErr != nil {
		r.logWarn("snapshot rollback connections failed", "group", group.Name, "node", owned, "error", snapshotErr)
	}
	latest, err := r.Core.Proxy(rollbackContext, group.Name)
	if err != nil {
		return fmt.Errorf("cannot verify rollback ownership for %s: %w", model.QuoteName(group.Name), err)
	}
	if latest.Now != owned {
		return nil
	}
	if latest.Type != "Selector" || !contains(latest.All, original) {
		return fmt.Errorf("%w: %s can no longer select the original member %s", ErrManualOverride, model.QuoteName(group.Name), model.QuoteName(original))
	}
	if selectErr := r.Core.Select(rollbackContext, group.Name, original); selectErr != nil {
		observed, readErr := r.Core.Proxy(rollbackContext, group.Name)
		if readErr != nil || observed.Now != original {
			return errors.Join(fmt.Errorf("rollback %s to %s: %w", model.QuoteName(group.Name), model.QuoteName(original), selectErr), readErr)
		}
	}
	if err := r.Core.CloseConnections(rollbackContext, connections); err != nil {
		return fmt.Errorf("cleanup after rollback: %w", err)
	}
	verified, err := r.Core.Proxy(rollbackContext, group.Name)
	if err != nil {
		return fmt.Errorf("verify rollback of %s: %w", model.QuoteName(group.Name), err)
	}
	if verified.Now != original {
		return fmt.Errorf("rollback readback for %s is %s, want %s", model.QuoteName(group.Name), model.QuoteName(verified.Now), model.QuoteName(original))
	}
	return nil
}

func (r *Runner) validateLiveDialer(ctx context.Context, group config.Group) error {
	if group.Mode == config.GroupModeSelector {
		return nil
	}
	chain, err := r.Core.Proxy(ctx, group.Chain)
	if err != nil {
		return fmt.Errorf("read live chain %s before changing %s: %w", model.QuoteName(group.Chain), model.QuoteName(group.Name), err)
	}
	if !physical(chain) || chain.Dialer != group.Name {
		return fmt.Errorf("live chain %s uses dialer-proxy %s, refusing to change %s", model.QuoteName(group.Chain), model.QuoteName(chain.Dialer), model.QuoteName(group.Name))
	}
	return nil
}

func (r *Runner) selectionConnections(ctx context.Context, group config.Group, old string) ([]string, error) {
	if group.Mode == config.GroupModeSelector {
		return nil, nil
	}
	return r.Core.ChainConnections(ctx, group.Chain, group.Name, old)
}

func (r *Runner) readSelectionAfterUncertainWrite(group string) (string, error) {
	readContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	proxy, err := r.Core.Proxy(readContext, group)
	return proxy.Now, err
}

func (r *Runner) closeConnectionsAfterUncertainWrite(group string, ids []string) {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Core.CloseConnections(cleanupContext, ids); err != nil {
		r.logWarn("connection cleanup after uncertain write failed", "group", group, "error", err)
	}
}

func geoFailure(result probe.GeoResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if result.Domain != "" {
		return "unexpected domain " + result.Domain
	}
	return "unexpected exit"
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (r *Runner) notifySwitch(group config.Group, oldNode string, result model.Sample, detail, eventID string) {
	if r.Notify == nil || !r.Cfg.Notifications.Enabled || !r.Cfg.Notifications.Switch {
		if r.NotifyEvent == nil || !r.Cfg.Notifications.Enabled || !r.Cfg.Notifications.Switch {
			return
		}
	}
	if r.NotifyEvent == nil && r.Notify == nil {
		return
	}
	reason := "链路故障恢复"
	if result.Kind == "confirm" {
		reason = "持续降速确认"
	} else if result.Kind == "bench" || result.Kind == "sweep" {
		reason = "测速选择"
	}
	body := fmt.Sprintf("%s：%s → %s，%s 验证通过", reason, oldNode, result.Node, group.ProbeTarget())
	if result.Mbps > 0 && result.Kind != "recovery" {
		body += fmt.Sprintf("，实测 %.1f Mbps", result.Mbps)
	}
	if detail != "" {
		body += "；" + detail
	}
	if r.NotifyEvent != nil {
		r.NotifyEvent("switch", group.Name+" 已切换", body, eventID)
	} else {
		r.Notify("switch", group.Name+" 已切换", body)
	}
}

func (r *Runner) logWarn(message string, args ...any) {
	if r.Log != nil {
		r.Log.Warn(message, args...)
	}
}
