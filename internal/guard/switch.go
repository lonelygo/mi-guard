package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

var ErrManualOverride = errors.New("proxy selection changed outside mihomo-guard")

// Apply changes the WARP dialer selector, validates the resulting chain, then
// mirrors the choice to the direct selector when configured. Every write is
// preceded by a fresh read so an operator's concurrent selection wins.
func (r *Runner) Apply(ctx context.Context, group config.Group, expectedCurrent string, ranked []model.Sample) (string, error) {
	if len(ranked) == 0 {
		return "", errors.New("no ranked candidates")
	}

	var selected string
	err := withSelectorLock(ctx, "apply", func() (applyErr error) {
		current, err := r.Core.Proxy(ctx, group.Name)
		if err != nil {
			return fmt.Errorf("read %q: %w", group.Name, err)
		}
		if current.Type != "Selector" {
			return fmt.Errorf("%q is %s, want Selector", group.Name, current.Type)
		}
		if expectedCurrent != "" && current.Now != expectedCurrent {
			return fmt.Errorf("%w: %q is now %q, expected %q", ErrManualOverride, group.Name, current.Now, expectedCurrent)
		}

		original := current.Now
		directExpected := ""
		if r.Cfg.Selection.SyncDirect && group.Direct != "" {
			if direct, readErr := r.Core.Proxy(ctx, group.Direct); readErr == nil {
				directExpected = direct.Now
			}
		}

		ownedSelection := original
		committed := false
		defer func() {
			if !committed && ownedSelection != original {
				applyErr = errors.Join(applyErr, r.rollbackSelection(group, original, ownedSelection))
			}
		}()
		var firstChainLive string
		var failures []string
		seen := make(map[string]struct{}, len(ranked))
		for _, result := range ranked {
			node := result.Node
			if node == "" {
				continue
			}
			if _, duplicate := seen[node]; duplicate {
				continue
			}
			seen[node] = struct{}{}
			if group.MinMbps > 0 && result.Mbps > 0 && result.Mbps < group.MinMbps {
				failures = append(failures, fmt.Sprintf("%s below %.1f Mbps", node, group.MinMbps))
				continue
			}
			if node == original {
				if ownedSelection != original {
					if restoreErr := r.restoreOriginal(ctx, group, original, ownedSelection); restoreErr != nil {
						return restoreErr
					}
					ownedSelection = original
				}
				if _, chainErr := r.Core.Delay(ctx, group.Chain, r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout); chainErr != nil {
					failures = append(failures, fmt.Sprintf("%s chain: %v", node, chainErr))
					continue
				}
				if firstChainLive == "" {
					firstChainLive = node
				}
				if group.GeoProxy != "" {
					geo, geoErr := r.checkGeo(ctx, group)
					if errors.Is(geoErr, probe.ErrRoute) {
						return fmt.Errorf("prove live geo route for %q: %w", group.Name, geoErr)
					}
					if !geo.OK {
						failures = append(failures, fmt.Sprintf("%s geo: %s", node, geoFailure(geo, geoErr)))
						continue
					}
					if geoErr != nil {
						r.logWarn("exit telemetry failed after Google region passed", "group", group.Name, "node", node, "error", geoErr)
					}
				}
				verified, readErr := r.Core.Proxy(ctx, group.Name)
				if readErr != nil {
					return fmt.Errorf("verify retained selection for %q: %w", group.Name, readErr)
				}
				if verified.Now != original {
					return fmt.Errorf("%w: %q changed from %q to %q during validation", ErrManualOverride, group.Name, original, verified.Now)
				}
				selected = original
				committed = true
				return nil
			}

			if _, delayErr := r.Core.Delay(ctx, node, r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout); delayErr != nil {
				failures = append(failures, fmt.Sprintf("%s unavailable", node))
				continue
			}
			latest, readErr := r.Core.Proxy(ctx, group.Name)
			if readErr != nil {
				return fmt.Errorf("re-read %q: %w", group.Name, readErr)
			}
			if latest.Now != ownedSelection {
				return fmt.Errorf("%w: %q changed from %q to %q", ErrManualOverride, group.Name, ownedSelection, latest.Now)
			}
			if dialerErr := r.validateLiveDialer(ctx, group); dialerErr != nil {
				return dialerErr
			}
			previous := ownedSelection
			connections, snapshotErr := r.Core.ChainConnections(ctx, group.Chain, group.Name, previous)
			if snapshotErr != nil {
				r.logWarn("snapshot chain connections failed", "group", group.Name, "node", previous, "error", snapshotErr)
			}
			if selectErr := r.Core.Select(ctx, group.Name, node); selectErr != nil {
				if observed, verifyErr := r.readSelectionAfterUncertainWrite(group.Name); verifyErr == nil && observed == node {
					ownedSelection = node
					r.closeConnectionsAfterUncertainWrite(group.Name, connections)
					return fmt.Errorf("select %q response failed after controller applied it: %w", node, selectErr)
				}
				failures = append(failures, fmt.Sprintf("%s select: %v", node, selectErr))
				continue
			}
			ownedSelection = node
			if cleanupErr := r.Core.CloseConnections(ctx, connections); cleanupErr != nil {
				r.logWarn("connection cleanup failed", "group", group.Name, "node", node, "error", cleanupErr)
			}

			if _, chainErr := r.Core.Delay(ctx, group.Chain, r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout); chainErr != nil {
				failures = append(failures, fmt.Sprintf("%s chain: %v", node, chainErr))
				continue
			}
			if firstChainLive == "" {
				firstChainLive = node
			}
			if group.GeoProxy != "" {
				geo, geoErr := r.checkGeo(ctx, group)
				if errors.Is(geoErr, probe.ErrRoute) {
					return fmt.Errorf("prove live geo route for %q: %w", group.Name, geoErr)
				}
				if !geo.OK {
					failures = append(failures, fmt.Sprintf("%s geo: %s", node, geoFailure(geo, geoErr)))
					continue
				}
				if geoErr != nil {
					r.logWarn("exit telemetry failed after Google region passed", "group", group.Name, "node", node, "error", geoErr)
				}
			}
			verified, readErr := r.Core.Proxy(ctx, group.Name)
			if readErr != nil {
				return fmt.Errorf("verify final selection for %q: %w", group.Name, readErr)
			}
			if verified.Now != ownedSelection {
				return fmt.Errorf("%w: %q changed from %q to %q during validation", ErrManualOverride, group.Name, ownedSelection, verified.Now)
			}

			selected = node
			detail := ""
			followed, mirrorErr := r.mirrorDirect(ctx, group, directExpected, node)
			if mirrorErr != nil {
				r.logWarn("direct group follow failed", "group", group.Direct, "node", node, "error", mirrorErr)
				detail = "直出组跟随未确认"
			} else if followed {
				detail = "直出组已跟随"
			}
			committed = true
			r.notifySwitch(group, original, result, detail)
			return nil
		}

		if !group.StrictGeo && firstChainLive != "" {
			if firstChainLive == original {
				selected = original
				committed = true
				return nil
			}
			latest, readErr := r.Core.Proxy(ctx, group.Name)
			if readErr != nil {
				return fmt.Errorf("re-read fallback %q: %w", group.Name, readErr)
			}
			if latest.Now != ownedSelection {
				return fmt.Errorf("%w: %q changed from %q to %q", ErrManualOverride, group.Name, ownedSelection, latest.Now)
			}
			if latest.Now != firstChainLive {
				if dialerErr := r.validateLiveDialer(ctx, group); dialerErr != nil {
					return dialerErr
				}
				previous := latest.Now
				connections, snapshotErr := r.Core.ChainConnections(ctx, group.Chain, group.Name, previous)
				if snapshotErr != nil {
					r.logWarn("snapshot fallback connections failed", "group", group.Name, "node", previous, "error", snapshotErr)
				}
				if selectErr := r.Core.Select(ctx, group.Name, firstChainLive); selectErr != nil {
					if observed, verifyErr := r.readSelectionAfterUncertainWrite(group.Name); verifyErr == nil && observed == firstChainLive {
						ownedSelection = firstChainLive
						r.closeConnectionsAfterUncertainWrite(group.Name, connections)
						return fmt.Errorf("fallback select %q response failed after controller applied it: %w", firstChainLive, selectErr)
					}
					return fmt.Errorf("select live fallback %q: %w", firstChainLive, selectErr)
				}
				ownedSelection = firstChainLive
				if cleanupErr := r.Core.CloseConnections(ctx, connections); cleanupErr != nil {
					r.logWarn("fallback connection cleanup failed", "group", group.Name, "node", firstChainLive, "error", cleanupErr)
				}
			}
			verified, verifyErr := r.Core.Proxy(ctx, group.Name)
			if verifyErr != nil {
				return fmt.Errorf("verify fallback selection for %q: %w", group.Name, verifyErr)
			}
			if verified.Now != firstChainLive {
				return fmt.Errorf("%w: %q changed to %q during fallback validation", ErrManualOverride, group.Name, verified.Now)
			}
			selected = firstChainLive
			detail := "Google 地区校验未通过，采用首个链路可用候选"
			followed, mirrorErr := r.mirrorDirect(ctx, group, directExpected, firstChainLive)
			if mirrorErr != nil {
				r.logWarn("direct group follow failed", "group", group.Direct, "node", firstChainLive, "error", mirrorErr)
				detail += "；直出组跟随未确认"
			} else if followed {
				detail += "；直出组已跟随"
			}
			committed = true
			r.notifySwitch(group, original, resultFor(firstChainLive, ranked), detail)
			return nil
		}

		if len(failures) == 0 {
			return errors.New("no alternative candidate")
		}
		return fmt.Errorf("no candidate passed validation: %s", strings.Join(failures, "; "))
	})
	return selected, err
}

func (r *Runner) mirrorDirect(ctx context.Context, group config.Group, expected, node string) (bool, error) {
	if !r.Cfg.Selection.SyncDirect || group.Direct == "" {
		return false, nil
	}
	if expected == "" {
		return false, errors.New("direct selector state was unavailable before primary switch")
	}
	direct, err := r.Core.Proxy(ctx, group.Direct)
	if err != nil {
		return false, err
	}
	if direct.Type != "Selector" {
		return false, fmt.Errorf("%q is %s, want Selector", group.Direct, direct.Type)
	}
	if direct.Now != expected {
		return false, fmt.Errorf("%w: %q is now %q, expected %q", ErrManualOverride, group.Direct, direct.Now, expected)
	}
	if !contains(direct.All, node) {
		return false, fmt.Errorf("%q is not a member of %q", node, group.Direct)
	}
	if err := r.Core.Select(ctx, group.Direct, node); err != nil {
		return false, err
	}
	verified, err := r.Core.Proxy(ctx, group.Direct)
	if err != nil {
		return false, err
	}
	if verified.Now != node {
		return false, fmt.Errorf("%q readback is %q after selecting %q", group.Direct, verified.Now, node)
	}
	return true, nil
}

func (r *Runner) rollbackSelection(group config.Group, original, owned string) error {
	rollbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	latest, err := r.Core.Proxy(rollbackContext, group.Name)
	if err != nil {
		return fmt.Errorf("cannot verify rollback ownership for %q: %w", group.Name, err)
	}
	if latest.Now != owned {
		return nil
	}
	if err := r.validateLiveDialer(rollbackContext, group); err != nil {
		return err
	}
	connections, snapshotErr := r.Core.ChainConnections(rollbackContext, group.Chain, group.Name, owned)
	if snapshotErr != nil {
		r.logWarn("snapshot rollback connections failed", "group", group.Name, "node", owned, "error", snapshotErr)
	}
	if selectErr := r.Core.Select(rollbackContext, group.Name, original); selectErr != nil {
		observed, readErr := r.Core.Proxy(rollbackContext, group.Name)
		if readErr != nil || observed.Now != original {
			return errors.Join(fmt.Errorf("rollback %q to %q: %w", group.Name, original, selectErr), readErr)
		}
	}
	if err := r.Core.CloseConnections(rollbackContext, connections); err != nil {
		return fmt.Errorf("cleanup after rollback: %w", err)
	}
	verified, err := r.Core.Proxy(rollbackContext, group.Name)
	if err != nil {
		return fmt.Errorf("verify rollback of %q: %w", group.Name, err)
	}
	if verified.Now != original {
		return fmt.Errorf("rollback readback for %q is %q, want %q", group.Name, verified.Now, original)
	}
	return nil
}

func (r *Runner) restoreOriginal(ctx context.Context, group config.Group, original, owned string) error {
	latest, err := r.Core.Proxy(ctx, group.Name)
	if err != nil {
		return fmt.Errorf("verify ownership before retaining %q: %w", original, err)
	}
	if latest.Now != owned {
		return fmt.Errorf("%w: %q changed from %q to %q", ErrManualOverride, group.Name, owned, latest.Now)
	}
	if err := r.validateLiveDialer(ctx, group); err != nil {
		return err
	}
	connections, snapshotErr := r.Core.ChainConnections(ctx, group.Chain, group.Name, owned)
	if snapshotErr != nil {
		r.logWarn("snapshot connections before retaining original failed", "group", group.Name, "node", owned, "error", snapshotErr)
	}
	if err := r.Core.Select(ctx, group.Name, original); err != nil {
		return fmt.Errorf("restore retained original %q: %w", original, err)
	}
	if err := r.Core.CloseConnections(ctx, connections); err != nil {
		r.logWarn("cleanup after retaining original failed", "group", group.Name, "error", err)
	}
	verified, err := r.Core.Proxy(ctx, group.Name)
	if err != nil {
		return fmt.Errorf("verify retained original %q: %w", original, err)
	}
	if verified.Now != original {
		return fmt.Errorf("retained original readback is %q, want %q", verified.Now, original)
	}
	return nil
}

func (r *Runner) validateLiveDialer(ctx context.Context, group config.Group) error {
	chain, err := r.Core.Proxy(ctx, group.Chain)
	if err != nil {
		return fmt.Errorf("read live chain %q before changing %q: %w", group.Chain, group.Name, err)
	}
	if chain.Dialer != group.Name {
		return fmt.Errorf("live chain %q uses dialer-proxy %q, refusing to change %q", group.Chain, chain.Dialer, group.Name)
	}
	return nil
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

func resultFor(node string, ranked []model.Sample) model.Sample {
	for _, result := range ranked {
		if result.Node == node {
			return result
		}
	}
	return model.Sample{Node: node}
}

func (r *Runner) notifySwitch(group config.Group, oldNode string, result model.Sample, detail string) {
	if r.Notify == nil || !r.Cfg.Notifications.Enabled || !r.Cfg.Notifications.Switch {
		return
	}
	reason := "链路故障恢复"
	if result.Kind == "confirm" {
		reason = "持续降速确认"
	} else if result.Kind == "bench" || result.Kind == "sweep" {
		reason = "测速选择"
	}
	body := fmt.Sprintf("%s：%s → %s，%s 验证通过", reason, oldNode, result.Node, group.Chain)
	if result.Mbps > 0 && result.Kind != "recovery" {
		body += fmt.Sprintf("，实测 %.1f Mbps", result.Mbps)
	}
	if detail != "" {
		body += "；" + detail
	}
	r.Notify("switch", group.Name+" 已切换", body)
}

func (r *Runner) logWarn(message string, args ...any) {
	if r.Log != nil {
		r.Log.Warn(message, args...)
	}
}
