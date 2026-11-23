package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
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
	ranked := withoutNode(historicCandidates(current, candidates, snapshot.Samples), current)
	for index := range ranked {
		ranked[index].Kind = "recovery"
	}
	selected, err := d.apply(ctx, group, current, ranked)
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
		return
	}
	baseline := policy.Baseline(current, currentSample.Provider, past)
	if !bandwidthDegraded(currentSample, baseline, group.MinMbps, d.runner.Cfg.Selection.SevereDropPct) {
		d.cancelPendingSlow(group.Name)
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
		d.confirmSlow(ctx, group, current, baseline, fresh, past, generation)
	}()
}

func (d *daemonRuntime) confirmSlow(ctx context.Context, group config.Group, current string, baseline float64, initialFresh, past []model.Sample, generation uint64) {
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
			return
		}
		if !bandwidthDegraded(currentResult, baseline, group.MinMbps, d.runner.Cfg.Selection.SevereDropPct) {
			return
		}
		confirmations++
	}

	if d.inCooldown(group.Name) {
		return
	}
	if !d.pendingMatches(group.Name, current, generation) {
		return
	}
	candidates, err := d.candidates(ctx, group.Name)
	if err != nil {
		return
	}
	seed := initialFresh
	sameSweep := len(seed) >= 2
	if !sameSweep {
		seed = cachedCandidates(candidates, past, d.runner.Cfg.Selection.MaxAge)
	}
	shortlist := alternativeNodes(current, policy.Rank(current, seed, past, d.runner.Cfg.Selection.NearBestPct), 2)
	shortlist = appendUnknownAlternatives(shortlist, current, candidates, 2)
	if len(shortlist) == 0 {
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
		return
	}
	if !d.pendingMatches(group.Name, current, generation) {
		return
	}
	selected, err := d.apply(ctx, group, current, ranked)
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

func (d *daemonRuntime) apply(ctx context.Context, group config.Group, expected string, ranked []model.Sample) (string, error) {
	if d.applyFn != nil {
		return d.applyFn(ctx, group, expected, ranked)
	}
	return d.runner.Apply(ctx, group, expected, ranked)
}

func (d *daemonRuntime) stableProxy(ctx context.Context, group string) (proxy model.Proxy, err error) {
	err = withSelectorLock(ctx, "apply", func() error {
		var readErr error
		proxy, readErr = d.runner.Core.Proxy(ctx, group)
		return readErr
	})
	return proxy, err
}

func historicCandidates(current string, candidates []string, samples []model.Sample) []model.Sample {
	latest := make(map[string]model.Sample, len(candidates))
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate] = struct{}{}
	}
	for _, sample := range samples {
		if _, ok := allowed[sample.Node]; !ok || sample.Mbps <= 0 || sample.Error != "" || sample.Kind == "heartbeat" || sample.Kind == "geo" {
			continue
		}
		if prior, ok := latest[sample.Node]; !ok || sample.At.After(prior.At) {
			latest[sample.Node] = sample
		}
	}
	known := make([]model.Sample, 0, len(candidates))
	unknown := make([]model.Sample, 0, len(candidates))
	for _, candidate := range candidates {
		if sample, ok := latest[candidate]; ok {
			known = append(known, sample)
		} else {
			unknown = append(unknown, model.Sample{Node: candidate})
		}
	}
	return append(policy.Rank(current, known, samples, 100), unknown...)
}

func cachedCandidates(candidates []string, samples []model.Sample, maxAge time.Duration) []model.Sample {
	if maxAge <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-maxAge)
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate] = struct{}{}
	}
	latest := make(map[string]model.Sample, len(candidates))
	for _, sample := range samples {
		if _, ok := allowed[sample.Node]; !ok || sample.At.Before(cutoff) || sample.Error != "" || sample.Mbps <= 0 || sample.Scope != "front" {
			continue
		}
		if prior, ok := latest[sample.Node]; !ok || sample.At.After(prior.At) {
			latest[sample.Node] = sample
		}
	}
	result := make([]model.Sample, 0, len(latest))
	for _, candidate := range candidates {
		if sample, ok := latest[candidate]; ok {
			result = append(result, sample)
		}
	}
	return result
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
