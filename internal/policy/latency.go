package policy

import (
	"math"

	"github.com/lonelygo/mi-guard/internal/model"
)

const (
	LatencyAboveLimit = "latencyAboveLimit"
	LatencyUnknown    = "latencyUnknown"
)

// LatencyGate excludes candidates whose current HTTP 204 latency is an
// outlier relative to completed prior rounds for the same group and probe.
type LatencyGate struct {
	MeanMS  float64
	Samples int
	Factor  float64
}

func (g LatencyGate) Threshold() (float64, bool) {
	if g.Samples < 1 || g.MeanMS <= 0 || math.IsNaN(g.MeanMS) || math.IsInf(g.MeanMS, 0) ||
		g.Factor < 1 || math.IsNaN(g.Factor) || math.IsInf(g.Factor, 0) {
		return 0, false
	}
	limit := min(g.MeanMS*g.Factor, math.MaxFloat64)
	// A decimal factor such as 1.15 can put an integral millisecond boundary
	// one floating-point step below its exact value.
	if rounded := math.Round(limit); math.Nextafter(limit, rounded) == rounded {
		limit = rounded
	}
	return limit, true
}

// Exclusion returns the policy reason for omitting an otherwise valid
// bandwidth candidate. Failed bandwidth measurements are left to Rank.
func (g LatencyGate) Exclusion(sample model.Sample) string {
	threshold, enabled := g.Threshold()
	if !enabled || !HasBandwidth(sample) {
		return ""
	}
	latency, known := sample.ProbeLatency()
	if !known {
		return LatencyUnknown
	}
	if float64(latency) > threshold {
		return LatencyAboveLimit
	}
	return ""
}

func (g LatencyGate) Filter(samples []model.Sample) []model.Sample {
	if _, enabled := g.Threshold(); !enabled {
		return samples
	}
	filtered := make([]model.Sample, 0, len(samples))
	for _, sample := range samples {
		if g.Exclusion(sample) == "" {
			filtered = append(filtered, sample)
		}
	}
	return filtered
}
