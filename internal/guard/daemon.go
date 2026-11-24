package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
	"github.com/lonelygo/mi-guard/internal/probe"
)

type daemonRuntime struct {
	runner *Runner

	mu     sync.Mutex
	groups map[string]*groupRuntime
	wg     sync.WaitGroup

	measureFn    func(context.Context, string, string, int64) (model.Sample, error)
	candidatesFn func(context.Context, string) ([]string, error)
	applyFn      func(context.Context, config.Group, string, []model.Sample) (string, error)
}

type groupRuntime struct {
	current       string
	heartFailures int
	geoFailures   int
	pendingSlow   bool
	cooldownUntil time.Time
	lastGeo       time.Time
	generation    uint64
}

// Daemon runs independent liveness and bandwidth schedulers. Liveness never
// waits for a full scan, so a long or budget-limited scan cannot delay recovery.
func (r *Runner) Daemon(ctx context.Context) error {
	if len(r.Cfg.Groups) == 0 {
		return errors.New("no managed groups configured")
	}
	runtime := &daemonRuntime{runner: r, groups: make(map[string]*groupRuntime, len(r.Cfg.Groups))}
	for _, group := range r.Cfg.Groups {
		runtime.groups[group.Name] = &groupRuntime{}
	}

	runtime.wg.Add(4)
	go runtime.heartbeatLoop(ctx)
	go runtime.fullScanLoop(ctx)
	go runtime.activeScanLoop(ctx)
	go runtime.geoLoop(ctx)
	runtime.wg.Wait()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

func (d *daemonRuntime) heartbeatLoop(ctx context.Context) {
	defer d.wg.Done()
	interval := positiveDuration(d.runner.Cfg.Schedule.HeartbeatInterval, 30*time.Second)
	backoff := time.Duration(0)
	for {
		if backoff > 0 {
			if !waitContext(ctx, backoff) {
				return
			}
		}
		hadControllerFailure := false
		for _, group := range d.runner.Cfg.Groups {
			if err := d.heartbeat(ctx, group); err != nil {
				if ctx.Err() != nil {
					return
				}
				d.runner.logWarn("heartbeat failed", "group", group.Name, "error", err)
				hadControllerFailure = true
			}
		}
		if hadControllerFailure {
			backoff = nextBackoff(backoff)
		} else {
			backoff = 0
		}
		if !waitContext(ctx, interval) {
			return
		}
	}
}

func (d *daemonRuntime) heartbeat(ctx context.Context, group config.Group) error {
	proxy, err := d.stableProxy(ctx, group.Name)
	if err != nil {
		return err
	}
	manual := d.observe(group.Name, proxy.Now)
	if manual {
		d.runner.logWarn("manual selection accepted", "group", group.Name, "node", proxy.Now)
	}

	started := time.Now()
	probeTarget := group.ProbeTarget()
	delay, delayErr := d.runner.Core.Delay(ctx, probeTarget, d.runner.Cfg.Speed.LivenessURL, d.runner.Cfg.Schedule.HeartbeatTimeout)
	sample := model.Sample{
		At:        time.Now().UTC(),
		Node:      proxy.Now,
		Scope:     probeTarget,
		Kind:      "heartbeat",
		ElapsedMS: time.Since(started).Milliseconds(),
		LatencyMS: delay,
		Attempts:  1,
	}
	if delayErr != nil {
		sample.Failures = 1
		sample.Error = delayErr.Error()
	}
	if recordErr := d.runner.Store.Record(ctx, sample); recordErr != nil {
		d.runner.logWarn("record heartbeat failed", "group", group.Name, "error", recordErr)
	}

	if delayErr == nil {
		d.setHeartFailures(group.Name, 0)
		return nil
	}
	failures := d.incrementHeartFailures(group.Name)
	threshold := d.runner.Cfg.Schedule.HeartbeatFailures
	if threshold <= 0 {
		threshold = 2
	}
	if failures < threshold {
		return nil
	}
	if err := d.recoverHard(ctx, group, proxy.Now); err != nil {
		return fmt.Errorf("recover after %d failures: %w", failures, err)
	}
	d.setHeartFailures(group.Name, 0)
	return nil
}

func (d *daemonRuntime) recoverHard(ctx context.Context, group config.Group, current string) error {
	candidates, err := d.candidates(ctx, group.Name)
	if err != nil {
		return err
	}
	snapshot, err := d.runner.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	known, unmeasured := d.candidateHistory(ctx, candidates, snapshot.Samples, time.Time{})
	ranked := policy.Rank(current, known, snapshot.Samples, 100)
	ranked = withoutNode(append(ranked, unmeasured...), current)
	for index := range ranked {
		ranked[index].Kind = "recovery"
	}
	selected, err := d.apply(ctx, group, current, ranked, ranked, snapshot.Samples)
	if err != nil {
		return err
	}
	d.markSwitched(group.Name, selected)
	return nil
}

func (d *daemonRuntime) fullScanLoop(ctx context.Context) {
	defer d.wg.Done()
	retry := minDuration(positiveDuration(d.runner.Cfg.Schedule.FullInterval, 12*time.Hour), time.Minute)
	for {
		delay, due := d.nextFullScan(ctx)
		if due {
			d.runFullScan(ctx, d.runner.Cfg.Groups)
			if !waitContext(ctx, retry) {
				return
			}
			continue
		}
		if !waitContext(ctx, delay) {
			return
		}
	}
}

func (d *daemonRuntime) nextFullScan(ctx context.Context) (time.Duration, bool) {
	interval := positiveDuration(d.runner.Cfg.Schedule.FullInterval, 12*time.Hour)
	snapshot, err := d.runner.Store.Snapshot(ctx)
	if err != nil {
		d.runner.logWarn("read history for scan schedule failed", "error", err)
		return time.Minute, false
	}
	last := snapshot.LastSweep
	if last.IsZero() || !last.Add(interval).After(time.Now()) {
		return 0, true
	}
	return time.Until(last.Add(interval)), false
}

func (d *daemonRuntime) runFullScan(ctx context.Context, groups []config.Group) {
	var summaries []string
	var measuredBytes int64
	var measured, attempted int
	started := false
	for _, group := range groups {
		if ctx.Err() != nil {
			return
		}
		proxy, err := d.stableProxy(ctx, group.Name)
		if err != nil {
			d.runner.logWarn("read group before sweep failed", "group", group.Name, "error", err)
			summaries = append(summaries, group.Name+"：无法读取当前节点")
			continue
		}
		d.observe(group.Name, proxy.Now)
		before, err := d.runner.Store.Snapshot(ctx)
		if err != nil {
			d.runner.logWarn("read history before sweep failed", "group", group.Name, "error", err)
			summaries = append(summaries, group.Name+"：无法读取测速历史")
			continue
		}
		candidates, err := d.candidates(ctx, group.Name)
		if err != nil {
			d.runner.logWarn("list sweep candidates failed", "group", group.Name, "error", err)
			summaries = append(summaries, group.Name+"：没有可测速候选")
			continue
		}
		if !started {
			if markErr := d.runner.Store.MarkSweep(ctx, time.Now().UTC()); markErr != nil {
				d.runner.logWarn("record full-sweep cadence failed", "error", markErr)
				return
			}
			started = true
		}
		fresh, err := d.runner.Scan(ctx, candidates, "sweep", d.runner.Cfg.Speed.SweepBytes)
		attempted += len(candidates)
		for _, sample := range fresh {
			measuredBytes += sample.Bytes
			if sample.Error == "" && sample.Mbps > 0 {
				measured++
			}
		}
		if err != nil {
			d.runner.logWarn("full sweep failed", "group", group.Name, "error", err)
			summaries = append(summaries, fmt.Sprintf("%s：测速中断，已完成 %d/%d", group.Name, len(fresh), len(candidates)))
			continue
		}
		ranked := policy.Rank(proxy.Now, fresh, before.Samples, d.runner.Cfg.Selection.NearBestPct)
		summaries = append(summaries, summarize(group.Name, proxy.Now, ranked))
		d.considerSlow(ctx, group, proxy.Now, fresh, before.Samples)
	}
	if started && len(summaries) > 0 && d.runner.Notify != nil && d.runner.Cfg.Notifications.Enabled && d.runner.Cfg.Notifications.Summary {
		body := fmt.Sprintf("%s；成功 %d/%d，下载 %.1f MiB", strings.Join(summaries, "；"), measured, attempted, float64(measuredBytes)/(1<<20))
		d.runner.Notify("summary", "mihomo-guard 测速完成", body)
	}
}

func (d *daemonRuntime) activeScanLoop(ctx context.Context) {
	defer d.wg.Done()
	interval := d.runner.Cfg.Schedule.ActiveInterval
	if interval <= 0 {
		<-ctx.Done()
		return
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for _, group := range d.runner.Cfg.Groups {
				d.runActiveCheck(ctx, group)
			}
			timer.Reset(interval)
		}
	}
}

func (d *daemonRuntime) geoLoop(ctx context.Context) {
	defer d.wg.Done()
	interval := d.runner.Cfg.Schedule.GeoInterval
	if interval <= 0 {
		<-ctx.Done()
		return
	}
	for {
		for _, group := range d.runner.Cfg.Groups {
			if group.GeoProxy == "" {
				continue
			}
			proxy, err := d.stableProxy(ctx, group.Name)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				d.runner.logWarn("read group for geo check failed", "group", group.Name, "error", err)
				continue
			}
			d.observe(group.Name, proxy.Now)
			d.maybeGeoCheck(ctx, group, proxy.Now)
		}
		if !waitContext(ctx, interval) {
			return
		}
	}
}

func (d *daemonRuntime) runActiveCheck(ctx context.Context, group config.Group) {
	proxy, err := d.stableProxy(ctx, group.Name)
	if err != nil {
		d.runner.logWarn("read active group failed", "group", group.Name, "error", err)
		return
	}
	d.observe(group.Name, proxy.Now)
	snapshot, err := d.runner.Store.Snapshot(ctx)
	if err != nil {
		d.runner.logWarn("read history for active check failed", "group", group.Name, "error", err)
		return
	}
	sample, err := d.measure(ctx, proxy.Now, "quick", d.runner.Cfg.Speed.QuickBytes)
	if err != nil {
		d.runner.logWarn("active speed check failed", "group", group.Name, "node", proxy.Now, "error", err)
		return
	}
	d.considerSlow(ctx, group, proxy.Now, []model.Sample{sample}, snapshot.Samples)
}

func (d *daemonRuntime) considerSlow(ctx context.Context, group config.Group, current string, fresh, past []model.Sample) {
	if !d.currentIs(group.Name, current) {
		return
	}
	currentSample, ok := sampleFor(current, fresh)
	if !ok {
		d.runner.recordDecisionOutcome(ctx, group, current, "skipped", "当前节点没有可比较测速结果", fresh)
		return
	}
	baseline := policy.Baseline(currentSample, past)
	if !bandwidthDegraded(currentSample, baseline, group.MinMbps, d.runner.Cfg.Selection.SevereDropPct) {
		d.cancelPendingSlow(group.Name)
		d.runner.recordDecisionOutcome(ctx, group, current, "kept", "", fresh)
		return
	}
	generation, started := d.startPendingSlow(group.Name)
	if !started {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer d.clearPendingSlow(group.Name, generation)
		d.confirmSlow(ctx, group, currentSample, baseline, fresh, past, generation)
	}()
}

func (d *daemonRuntime) confirmSlow(ctx context.Context, group config.Group, initial model.Sample, baseline float64, initialFresh, past []model.Sample, generation uint64) {
	current := initial.Node
	required := d.runner.Cfg.Selection.SlowConfirmations
	if required < 2 {
		required = 2
	}
	confirmations := 1
	var currentResult model.Sample
	for confirmations < required {
		if !waitContext(ctx, positiveDuration(d.runner.Cfg.Schedule.RecheckDelay, time.Minute)) {
			return
		}
		if !d.pendingMatches(group.Name, current, generation) {
			return
		}
		latest, err := d.stableProxy(ctx, group.Name)
		if err != nil || latest.Now != current {
			return
		}
		currentResult, err = d.measure(ctx, current, "confirm", d.runner.Cfg.Speed.ConfirmBytes)
		if err != nil {
			d.runner.recordDecisionOutcome(ctx, group, current, "failed", "复测未完成", append([]model.Sample{initial}, currentResult))
			return
		}
		if !policy.Comparable(initial, currentResult) || !bandwidthDegraded(currentResult, baseline, group.MinMbps, d.runner.Cfg.Selection.SevereDropPct) {
			d.runner.recordDecisionOutcome(ctx, group, current, "kept", "复测未确认持续降速", append([]model.Sample{initial}, currentResult))
			return
		}
		confirmations++
	}

	if d.inCooldown(group.Name) {
		d.runner.recordDecisionOutcome(ctx, group, current, "skipped", "处于切换冷静期", []model.Sample{currentResult})
		return
	}
	if !d.pendingMatches(group.Name, current, generation) {
		return
	}
	candidates, err := d.candidates(ctx, group.Name)
	if err != nil {
		d.runner.recordDecisionOutcome(ctx, group, current, "failed", "无法读取候选节点", []model.Sample{currentResult})
		return
	}
	seed := initialFresh
	sameSweep := len(seed) >= 2
	if !sameSweep {
		seed = nil
		if age := d.runner.Cfg.Selection.MaxAge; age > 0 {
			seed, _ = d.candidateHistory(ctx, candidates, past, time.Now().Add(-age))
		}
	}
	shortlist := alternativeNodes(current, policy.Rank(current, seed, past, d.runner.Cfg.Selection.NearBestPct), 2)
	shortlist = appendUnknownAlternatives(shortlist, current, candidates, 2)
	if len(shortlist) == 0 {
		d.runner.recordDecisionOutcome(ctx, group, current, "kept", "没有可复测的备选节点", []model.Sample{currentResult})
		return
	}
	confirmed := []model.Sample{currentResult}
	for _, node := range shortlist {
		if result, ok := sampleFor(node, seed); sameSweep && ok {
			confirmed = append(confirmed, result)
			continue
		}
		result, measureErr := d.measure(ctx, node, "confirm", d.runner.Cfg.Speed.ConfirmBytes)
		if measureErr == nil {
			confirmed = append(confirmed, result)
		}
	}
	ranked := policy.Rank(current, confirmed, past, d.runner.Cfg.Selection.NearBestPct)
	forceReplacement := currentResult.Error != "" || currentResult.Mbps <= 0 || group.MinMbps > 0 && currentResult.Mbps < group.MinMbps
	if forceReplacement {
		ranked = withoutNode(ranked, current)
	}
	if !hasBetterAlternative(current, currentResult.Mbps, ranked, group.MinMbps) {
		d.runner.recordDecisionOutcome(ctx, group, current, "kept", "没有满足切换条件的更优候选", confirmed)
		return
	}
	if !d.pendingMatches(group.Name, current, generation) {
		return
	}
	selected, err := d.apply(ctx, group, current, ranked, confirmed, past)
	if err != nil {
		d.runner.logWarn("slow-path switch failed", "group", group.Name, "error", err)
		return
	}
	d.markSwitched(group.Name, selected)
}

func (d *daemonRuntime) maybeGeoCheck(ctx context.Context, group config.Group, current string) {
	if group.GeoProxy == "" || d.runner.Cfg.Schedule.GeoInterval <= 0 {
		return
	}
	if !d.geoDue(group.Name, d.runner.Cfg.Schedule.GeoInterval) {
		return
	}
	result, geoErr := d.runner.checkGeo(ctx, group)
	d.markGeoChecked(group.Name)
	if errors.Is(geoErr, probe.ErrRoute) {
		d.runner.logWarn("geo route proof failed", "group", group.Name, "error", geoErr)
		return
	}
	sample := model.Sample{At: time.Now().UTC(), Node: current, Scope: group.ProbeTarget(), Kind: "geo", Attempts: 1}
	if !result.OK {
		sample.Failures = 1
		sample.Error = geoFailure(result, geoErr)
	} else if geoErr != nil {
		d.runner.logWarn("exit telemetry failed after Google region passed", "group", group.Name, "error", geoErr)
	}
	if sample.Failures == 0 {
		d.setGeoFailures(group.Name, 0)
		return
	}
	if d.incrementGeoFailures(group.Name) < max(2, d.runner.Cfg.Schedule.HeartbeatFailures) || d.inCooldown(group.Name) {
		return
	}
	if err := d.recoverHard(ctx, group, current); err != nil {
		d.runner.logWarn("geo recovery failed", "group", group.Name, "error", err)
	}
}

func (d *daemonRuntime) observe(group, current string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.groups[group]
	manual := state.current != "" && state.current != current
	if manual {
		state.heartFailures = 0
		state.geoFailures = 0
		state.pendingSlow = false
		state.generation++
	}
	state.current = current
	return manual
}

func (d *daemonRuntime) markSwitched(group, node string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.groups[group]
	state.current = node
	state.heartFailures = 0
	state.geoFailures = 0
	state.generation++
	if d.runner.Cfg.Schedule.Cooldown > 0 {
		state.cooldownUntil = time.Now().Add(d.runner.Cfg.Schedule.Cooldown)
	} else {
		state.cooldownUntil = time.Time{}
	}
}

func (d *daemonRuntime) incrementHeartFailures(group string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[group].heartFailures++
	return d.groups[group].heartFailures
}

func (d *daemonRuntime) setHeartFailures(group string, value int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[group].heartFailures = value
}

func (d *daemonRuntime) incrementGeoFailures(group string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[group].geoFailures++
	return d.groups[group].geoFailures
}

func (d *daemonRuntime) setGeoFailures(group string, value int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[group].geoFailures = value
}

func (d *daemonRuntime) inCooldown(group string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Now().Before(d.groups[group].cooldownUntil)
}

func (d *daemonRuntime) startPendingSlow(group string) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.groups[group].pendingSlow {
		return 0, false
	}
	d.groups[group].pendingSlow = true
	d.groups[group].generation++
	return d.groups[group].generation, true
}

func (d *daemonRuntime) clearPendingSlow(group string, generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.groups[group].generation == generation {
		d.groups[group].pendingSlow = false
	}
}

func (d *daemonRuntime) cancelPendingSlow(group string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.groups[group]
	if state.pendingSlow {
		state.pendingSlow = false
		state.generation++
	}
}

func (d *daemonRuntime) pendingMatches(group, current string, generation uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.groups[group]
	return state.pendingSlow && state.generation == generation && state.current == current
}

func (d *daemonRuntime) currentIs(group, current string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.groups[group].current == current
}

func (d *daemonRuntime) geoDue(group string, interval time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.groups[group].lastGeo.IsZero() || time.Since(d.groups[group].lastGeo) >= interval
}

func (d *daemonRuntime) markGeoChecked(group string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[group].lastGeo = time.Now()
}

func (d *daemonRuntime) measure(ctx context.Context, node, kind string, limit int64) (model.Sample, error) {
	if d.measureFn != nil {
		return d.measureFn(ctx, node, kind, limit)
	}
	return d.runner.Measure(ctx, node, kind, limit)
}

func (d *daemonRuntime) candidates(ctx context.Context, group string) ([]string, error) {
	if d.candidatesFn != nil {
		return d.candidatesFn(ctx, group)
	}
	return d.runner.Candidates(ctx, group)
}

func (d *daemonRuntime) apply(ctx context.Context, group config.Group, expected string, ranked, evidence, past []model.Sample) (string, error) {
	if d.applyFn != nil {
		return d.applyFn(ctx, group, expected, ranked)
	}
	results, err := d.runner.applySelections(ctx, BenchTarget{Group: group, Current: expected, Evidence: evidence, History: past}, ranked, false)
	if len(results) == 0 {
		return "", err
	}
	if results[0].Action == "skipped" {
		return "", errors.Join(errors.New(results[0].Reason), err)
	}
	return results[0].After, err
}

func (d *daemonRuntime) stableProxy(ctx context.Context, group string) (model.Proxy, error) {
	proxy, _, err := d.runner.ObservePrimary(ctx, group)
	if err != nil && proxy.Now != "" {
		// The live read succeeded. Keep liveness checks running on a history I/O failure.
		d.runner.logWarn("record selection observation failed", "group", group, "error", err)
		return proxy, nil
	}
	return proxy, err
}

// ObservePrimary reads and records a stable primary while guard switches and
// rollbacks are excluded. It also returns the live proxy if persistence fails.
func (r *Runner) ObservePrimary(ctx context.Context, group string) (proxy model.Proxy, tenure history.SelectionTenure, err error) {
	err = withSelectorLock(ctx, "apply", func() error {
		p, err := r.Core.Proxy(ctx, group)
		if err != nil {
			return err
		}
		if p.Type != "Selector" {
			return fmt.Errorf("invalid live target %s: got %s, want Selector", group, p.Type)
		}
		proxy = p
		if p.Now == "" {
			return nil
		}
		tenure, err = r.Store.RecordSelection(ctx, group, p.Now, time.Now().UTC(), false)
		return err
	})
	return proxy, tenure, err
}

func (d *daemonRuntime) candidateHistory(ctx context.Context, candidates []string, past []model.Sample, cutoff time.Time) ([]model.Sample, []model.Sample) {
	var inventory map[string]model.Proxy
	if len(past) > 0 {
		// Candidate names can move between providers or become dialer-backed.
		// If identity cannot be checked, the candidates still get live verification.
		var err error
		inventory, err = d.runner.Core.Inventory(ctx)
		if err != nil {
			d.runner.logWarn("read candidate routes for history failed", "error", err)
		}
	}
	profile := d.runner.Cfg.DownloadProfile(1, d.runner.Cfg.Speed.ConfirmBytes)
	return daemonCandidateSamples(candidates, inventory, past, profile, cutoff)
}

func daemonCandidateSamples(candidates []string, inventory map[string]model.Proxy, past []model.Sample, profile model.DownloadProfile, cutoff time.Time) (known, unmeasured []model.Sample) {
	routes := make(map[string]model.Sample, len(candidates))
	for _, node := range candidates {
		proxy, ok := inventory[node]
		if !ok || !physical(proxy) {
			continue
		}
		scope := "front"
		if proxy.Dialer != "" {
			scope = "chain:" + node
		}
		routes[node] = model.Sample{Node: node, Provider: proxy.Provider, Scope: scope, Kind: "confirm", Profile: profile}
	}
	latest := make(map[string]model.Sample, len(candidates))
	for _, sample := range past {
		route, ok := routes[sample.Node]
		if !ok || sample.At.Before(cutoff) || !policy.HasBandwidth(sample) || !policy.Comparable(route, sample) {
			continue
		}
		if prior, ok := latest[sample.Node]; !ok || sample.At.After(prior.At) {
			latest[sample.Node] = sample
		}
	}
	for _, node := range candidates {
		if sample, ok := latest[node]; ok {
			known = append(known, sample)
		} else {
			unmeasured = append(unmeasured, model.Sample{Node: node})
		}
	}
	return known, unmeasured
}

func withoutNode(samples []model.Sample, node string) []model.Sample {
	result := make([]model.Sample, 0, len(samples))
	for _, sample := range samples {
		if sample.Node != node {
			result = append(result, sample)
		}
	}
	return result
}

func alternativeNodes(current string, ranked []model.Sample, limit int) []string {
	result := make([]string, 0, limit)
	for _, sample := range ranked {
		if sample.Node == "" || sample.Node == current || sample.Error != "" || sample.Mbps <= 0 {
			continue
		}
		result = append(result, sample.Node)
		if len(result) == limit {
			break
		}
	}
	return result
}

func appendUnknownAlternatives(selected []string, current string, candidates []string, limit int) []string {
	seen := make(map[string]struct{}, len(selected)+1)
	seen[current] = struct{}{}
	for _, node := range selected {
		seen[node] = struct{}{}
	}
	for _, candidate := range candidates {
		if len(selected) >= limit {
			break
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		selected = append(selected, candidate)
		seen[candidate] = struct{}{}
	}
	return selected
}

func bandwidthDegraded(sample model.Sample, baseline, minimum, dropPct float64) bool {
	if sample.Error != "" || sample.Mbps <= 0 {
		return true
	}
	if minimum > 0 && sample.Mbps < minimum {
		return true
	}
	return baseline > 0 && policy.Dropped(sample.Mbps, baseline, dropPct)
}

func hasBetterAlternative(current string, currentMbps float64, ranked []model.Sample, minimum float64) bool {
	for _, sample := range ranked {
		if sample.Node == current {
			return false
		}
		if sample.Error == "" && sample.Mbps > currentMbps && (minimum <= 0 || sample.Mbps >= minimum) {
			return true
		}
	}
	return false
}

func sampleFor(node string, samples []model.Sample) (model.Sample, bool) {
	for _, sample := range samples {
		if sample.Node == node {
			return sample, true
		}
	}
	return model.Sample{}, false
}

func summarize(group, current string, ranked []model.Sample) string {
	if len(ranked) == 0 {
		return group + "：没有成功结果"
	}
	currentResult, ok := sampleFor(current, ranked)
	if ok {
		return fmt.Sprintf("%s：当前 %s %.1f Mbps，推荐 %s %.1f Mbps", group, current, currentResult.Mbps, ranked[0].Node, ranked[0].Mbps)
	}
	return fmt.Sprintf("%s：当前 %s 无有效结果，推荐 %s %.1f Mbps", group, current, ranked[0].Node, ranked[0].Mbps)
}

func positiveDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(previous time.Duration) time.Duration {
	switch {
	case previous < 5*time.Second:
		return 5 * time.Second
	case previous < 15*time.Second:
		return 15 * time.Second
	default:
		return 30 * time.Second
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
