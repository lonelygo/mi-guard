// Package policy implements bandwidth-drop detection and stable candidate
// selection from fresh and historical observations.
package policy

import (
	"math"
	"sort"

	"github.com/lonelygo/mi-guard/internal/model"
)

// Baseline returns the median of preceding comparable bandwidth samples.
// Quick probes can trigger a comparison, but their smaller transfers do not
// contribute to the baseline.
func Baseline(sample model.Sample, past []model.Sample) float64 {
	values := statsFor(sample, past).bandwidth
	if len(values) == 0 {
		return 0
	}
	return percentile(values, 0.5)
}

// Dropped reports whether now has fallen by at least pct from baseline. The
// boundary is inclusive, so baseline=100, pct=20, now=80 starts preparation.
func Dropped(now, baseline, pct float64) bool {
	if !finitePositive(now) || !finitePositive(baseline) || math.IsNaN(pct) || pct < 0 || pct >= 100 {
		return false
	}
	return now <= baseline*(1-pct/100)
}

// Rank orders valid fresh observations. Candidates within tolerance of the
// fastest result form the preferred band and are ranked by observed request
// reliability, historical lower-quartile bandwidth, current-node stickiness,
// then latency. Slower results remain usable fallbacks ordered by current speed.
func Rank(current string, fresh, past []model.Sample, tolerance float64) []model.Sample {
	latest := latestBandwidthSamples(fresh)
	if len(latest) == 0 {
		return nil
	}
	threshold, _ := PreferredThreshold(fresh, tolerance)
	preferred := make([]ranked, 0, len(latest))
	fallback := make([]ranked, 0, len(latest))
	for _, sample := range latest {
		stats := statsFor(sample, past)
		item := ranked{
			sample:  sample,
			riskNum: int64(stats.failures + 1),
			riskDen: int64(stats.attempts + 2),
			current: sample.Node == current,
		}
		if len(stats.bandwidth) >= 3 {
			item.historyKnown = true
			item.historyLow = percentile(stats.bandwidth, 0.25)
		}
		if sample.Mbps >= threshold {
			preferred = append(preferred, item)
		} else {
			fallback = append(fallback, item)
		}
	}
	sort.SliceStable(preferred, func(i, j int) bool { return preferredLess(preferred[i], preferred[j]) })
	sort.SliceStable(fallback, func(i, j int) bool { return fallbackLess(fallback[i], fallback[j]) })
	result := make([]model.Sample, 0, len(latest))
	for _, item := range append(preferred, fallback...) {
		result = append(result, item.sample)
	}
	return result
}

// PreferredThreshold returns the exact near-best boundary used by Rank for
// the latest valid observation of each candidate identity.
func PreferredThreshold(fresh []model.Sample, tolerance float64) (float64, bool) {
	latest := latestBandwidthSamples(fresh)
	if len(latest) == 0 {
		return 0, false
	}
	if math.IsNaN(tolerance) || tolerance < 0 {
		tolerance = 0
	}
	if tolerance > 100 {
		tolerance = 100
	}
	best := 0.0
	for _, sample := range latest {
		best = max(best, sample.Mbps)
	}
	return best * (1 - tolerance/100), true
}

func latestBandwidthSamples(fresh []model.Sample) map[string]model.Sample {
	latest := make(map[string]model.Sample)
	order := make(map[string]int)
	for index, sample := range fresh {
		if !HasBandwidth(sample) {
			continue
		}
		key := identity(sample)
		prior, exists := latest[key]
		if !exists || prior.At.Before(sample.At) || prior.At.Equal(sample.At) && index > order[key] {
			latest[key] = sample
			order[key] = index
		}
	}
	return latest
}

type ranked struct {
	sample       model.Sample
	riskNum      int64
	riskDen      int64
	historyLow   float64
	historyKnown bool
	current      bool
}

func preferredLess(left, right ranked) bool {
	if comparison := compareRisk(left, right); comparison != 0 {
		return comparison < 0
	}
	if left.historyKnown != right.historyKnown {
		return left.historyKnown
	}
	if left.historyKnown && left.historyLow != right.historyLow {
		return left.historyLow > right.historyLow
	}
	if left.current != right.current {
		return left.current
	}
	if left.sample.LatencyMS != right.sample.LatencyMS {
		if left.sample.LatencyMS <= 0 {
			return false
		}
		if right.sample.LatencyMS <= 0 {
			return true
		}
		return left.sample.LatencyMS < right.sample.LatencyMS
	}
	if left.sample.Mbps != right.sample.Mbps {
		return left.sample.Mbps > right.sample.Mbps
	}
	return identity(left.sample) < identity(right.sample)
}

func fallbackLess(left, right ranked) bool {
	if left.sample.Mbps != right.sample.Mbps {
		return left.sample.Mbps > right.sample.Mbps
	}
	if comparison := compareRisk(left, right); comparison != 0 {
		return comparison < 0
	}
	return identity(left.sample) < identity(right.sample)
}

// HasBandwidth reports whether a named node produced a successful finite speed.
func HasBandwidth(sample model.Sample) bool {
	return sample.Node != "" && sample.Error == "" && finitePositive(sample.Mbps)
}

func daemonKind(kind string) bool {
	return kind == "sweep" || kind == "quick" || kind == "confirm"
}

// Comparable reports whether two measurements belong to the same history.
// Benchmarks require identical profiles. Daemon quick, sweep and confirmation
// probes intentionally share evidence across their single-flow transfer sizes.
func Comparable(fresh, old model.Sample) bool {
	if fresh.Node != old.Node || fresh.Provider != old.Provider || fresh.Scope == "" || fresh.Scope != old.Scope ||
		!fresh.Profile.Known() || !old.Profile.Known() {
		return false
	}
	if fresh.Kind == "bench" {
		return old.Kind == "bench" && fresh.Profile == old.Profile
	}
	return daemonKind(fresh.Kind) && daemonKind(old.Kind) && fresh.Profile.Streams == 1 &&
		old.Profile.Streams == 1 && fresh.Profile.Target == old.Profile.Target
}

type sampleStats struct {
	bandwidth    []float64
	attempts     int
	failures     int
	pastAttempts int
	pastFailures int
}

// HistoryEvidence exposes the exact comparable-history inputs used by Rank.
// Attempts and failures include the fresh candidate just as the ranking risk
// does. P25 is available only after the same three-bandwidth-sample gate.
type HistoryEvidence struct {
	Attempts       int
	Failures       int
	PastAttempts   int
	PastFailures   int
	BandwidthCount int
	P25Mbps        float64
	P25Known       bool
}

func ExplainHistory(sample model.Sample, past []model.Sample) HistoryEvidence {
	stats := statsFor(sample, past)
	evidence := HistoryEvidence{Attempts: stats.attempts, Failures: stats.failures,
		PastAttempts: stats.pastAttempts, PastFailures: stats.pastFailures, BandwidthCount: len(stats.bandwidth)}
	if len(stats.bandwidth) >= 3 {
		evidence.P25Mbps = percentile(stats.bandwidth, 0.25)
		evidence.P25Known = true
	}
	return evidence
}

const maxReliabilityObservations = 1_000_000_000

func statsFor(sample model.Sample, past []model.Sample) sampleStats {
	var stats sampleStats
	seen := make(map[sampleFingerprint]struct{})
	fresh := fingerprintOf(sample)
	for _, old := range past {
		if !Comparable(sample, old) {
			continue
		}
		id := fingerprintOf(old)
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if old.Kind != "quick" && HasBandwidth(old) {
			stats.bandwidth = append(stats.bandwidth, old.Mbps)
		}
		if id != fresh {
			attempts := min(max(old.Attempts, 0), maxReliabilityObservations)
			failures := min(max(old.Failures, 0), attempts)
			stats.pastAttempts = min(stats.pastAttempts+attempts, maxReliabilityObservations)
			stats.pastFailures = min(stats.pastFailures+failures, stats.pastAttempts)
			stats.addProbes(old)
		}
	}
	if sample.Kind == "bench" || daemonKind(sample.Kind) {
		stats.addProbes(sample)
	}
	sort.Float64s(stats.bandwidth)
	return stats
}

func (s *sampleStats) addProbes(sample model.Sample) {
	attempts := min(max(sample.Attempts, 0), maxReliabilityObservations)
	failures := min(max(sample.Failures, 0), attempts)
	s.attempts = min(s.attempts+attempts, maxReliabilityObservations)
	s.failures = min(s.failures+failures, s.attempts)
}

type sampleFingerprint struct {
	at           int64
	node         string
	provider     string
	scope        string
	kind         string
	profile      model.DownloadProfile
	mbps         uint64
	bytes        int64
	elapsed      int64
	latency      int
	attempts     int
	failures     int
	errorMessage string
}

func fingerprintOf(sample model.Sample) sampleFingerprint {
	return sampleFingerprint{
		at:           sample.At.UnixNano(),
		node:         sample.Node,
		provider:     sample.Provider,
		scope:        sample.Scope,
		kind:         sample.Kind,
		profile:      sample.Profile,
		mbps:         math.Float64bits(sample.Mbps),
		bytes:        sample.Bytes,
		elapsed:      sample.ElapsedMS,
		latency:      sample.LatencyMS,
		attempts:     sample.Attempts,
		failures:     sample.Failures,
		errorMessage: sample.Error,
	}
}

func compareRisk(left, right ranked) int {
	leftValue := left.riskNum * right.riskDen
	rightValue := right.riskNum * left.riskDen
	switch {
	case leftValue < rightValue:
		return -1
	case leftValue > rightValue:
		return 1
	default:
		return 0
	}
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func identity(sample model.Sample) string {
	return sample.Provider + "\x00" + sample.Node
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
