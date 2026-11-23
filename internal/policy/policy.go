// Package policy implements bandwidth-drop detection and stable candidate
// selection from fresh and historical observations.
package policy

import (
	"math"
	"sort"

	"github.com/lonelygo/mi-guard/internal/model"
)

// Baseline returns the median of preceding comparable bandwidth samples.
// Quick samples are deliberately excluded because their smaller transfer size
// is biased. Failed downloads and measurements from other routes or providers
// are not comparable.
func Baseline(node, provider string, samples []model.Sample) float64 {
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.Node != node || sample.Provider != provider || !historical(sample) {
			continue
		}
		values = append(values, sample.Mbps)
	}
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
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
	latest := make(map[string]model.Sample)
	order := make(map[string]int)
	for index, sample := range fresh {
		if !freshValid(sample) {
			continue
		}
		key := identity(sample)
		prior, exists := latest[key]
		if !exists || prior.At.Before(sample.At) || prior.At.Equal(sample.At) && index > order[key] {
			latest[key] = sample
			order[key] = index
		}
	}
	if len(latest) == 0 {
		return nil
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
	threshold := best * (1 - tolerance/100)
	preferred := make([]ranked, 0, len(latest))
	fallback := make([]ranked, 0, len(latest))
	for _, sample := range latest {
		reliability := aggregateReliability(sample, past)
		item := ranked{
			sample:       sample,
			riskNum:      int64(reliability.failures + 1),
			riskDen:      int64(reliability.attempts + 2),
			historyLow:   historicalLow(sample, past),
			historyKnown: historicalCount(sample, past) >= 3,
			current:      sample.Node == current,
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

func historical(sample model.Sample) bool {
	if sample.Scope != "front" || sample.Error != "" || !finitePositive(sample.Mbps) {
		return false
	}
	switch sample.Kind {
	case "sweep", "bench", "confirm":
		return true
	default:
		return false
	}
}

func freshValid(sample model.Sample) bool {
	return sample.Node != "" && sample.Error == "" && finitePositive(sample.Mbps)
}

func historicalLow(sample model.Sample, past []model.Sample) float64 {
	values := make([]float64, 0)
	for _, old := range past {
		if old.Node == sample.Node && old.Provider == sample.Provider && historical(old) {
			values = append(values, old.Mbps)
		}
	}
	if len(values) < 3 {
		return 0
	}
	sort.Float64s(values)
	return percentile(values, 0.25)
}

func historicalCount(sample model.Sample, past []model.Sample) int {
	count := 0
	for _, old := range past {
		if sameRoute(sample, old) && historical(old) {
			count++
		}
	}
	return count
}

type reliability struct {
	attempts int
	failures int
}

const maxReliabilityObservations = 1_000_000_000

func aggregateReliability(sample model.Sample, past []model.Sample) reliability {
	result := reliability{}
	seen := make(map[sampleFingerprint]struct{}, len(past)+1)
	observations := make([]model.Sample, 0, len(past)+1)
	observations = append(observations, past...)
	observations = append(observations, sample)
	for _, observation := range observations {
		if !sameRoute(sample, observation) || !probeComparable(observation) {
			continue
		}
		fingerprint := fingerprintOf(observation)
		if _, duplicate := seen[fingerprint]; duplicate {
			continue
		}
		seen[fingerprint] = struct{}{}
		attempts := min(max(observation.Attempts, 0), maxReliabilityObservations)
		failures := min(max(observation.Failures, 0), attempts)
		result.attempts = min(result.attempts+attempts, maxReliabilityObservations)
		result.failures = min(result.failures+failures, result.attempts)
	}
	return result
}

type sampleFingerprint struct {
	at           int64
	node         string
	provider     string
	scope        string
	kind         string
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
		mbps:         math.Float64bits(sample.Mbps),
		bytes:        sample.Bytes,
		elapsed:      sample.ElapsedMS,
		latency:      sample.LatencyMS,
		attempts:     sample.Attempts,
		failures:     sample.Failures,
		errorMessage: sample.Error,
	}
}

func sameRoute(fresh, observation model.Sample) bool {
	return fresh.Node == observation.Node && fresh.Provider == observation.Provider && observation.Scope == "front"
}

func probeComparable(sample model.Sample) bool {
	if sample.Scope != "front" {
		return false
	}
	switch sample.Kind {
	case "sweep", "bench", "confirm", "quick":
		return true
	default:
		return false
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
