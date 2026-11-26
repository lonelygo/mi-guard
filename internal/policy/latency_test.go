package policy

import (
	"math"
	"testing"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestLatencyGateFiltersBeforeBandwidthRanking(t *testing.T) {
	fastOutlier := sample("fast-outlier", 100, 2, 0)
	fastOutlier.LatencyMS = 151
	steady := sample("steady", 82, 2, 0)
	steady.LatencyMS = 150
	slow := sample("slow", 65, 2, 0)
	slow.LatencyMS = 40

	gate := LatencyGate{MeanMS: 100, Samples: 12, Factor: 1.5}
	filtered := gate.Filter([]model.Sample{fastOutlier, steady, slow})
	assertNodes(t, Rank("", filtered, nil, 20), "steady", "slow")
	if threshold, ok := PreferredThreshold(filtered, 20); !ok || math.Abs(threshold-65.6) > 1e-9 {
		t.Fatalf("filtered bandwidth threshold = %v, %v; want 65.6, true", threshold, ok)
	}
	if got := gate.Exclusion(fastOutlier); got != LatencyAboveLimit {
		t.Fatalf("outlier exclusion = %q", got)
	}
	if got := gate.Exclusion(steady); got != "" {
		t.Fatalf("boundary exclusion = %q", got)
	}
}

func TestLatencyGateUnknownAndFailedMeasurements(t *testing.T) {
	gate := LatencyGate{MeanMS: 100, Samples: 3, Factor: 1.5}
	unknown := sample("unknown", 90, 1, 0)
	unknown.LatencyMS = 0
	if got := gate.Exclusion(unknown); got != LatencyUnknown {
		t.Fatalf("unknown exclusion = %q", got)
	}
	failed := withError(sample("failed", 90, 1, 1))
	failed.LatencyMS = 1000
	if got := gate.Exclusion(failed); got != "" {
		t.Fatalf("failed measurement was misclassified as latency: %q", got)
	}
	downstreamFailure := withError(sample("download-failed", 0, 2, 1))
	downstreamFailure.LatencyMS = 1000
	if got := gate.Exclusion(downstreamFailure); got != "" {
		t.Fatalf("download failure was misclassified as latency: %q", got)
	}
}

func TestLatencyGateConfigurableFactorAtIntegerBoundary(t *testing.T) {
	gate := LatencyGate{MeanMS: 200, Samples: 2, Factor: 1.15}
	boundary := sample("boundary", 100, 2, 0)
	boundary.LatencyMS = 230
	if limit, ok := gate.Threshold(); !ok || limit != 230 || gate.Exclusion(boundary) != "" {
		t.Fatalf("boundary excluded or misreported at %.17g ms", limit)
	}
	gate.Factor = math.MaxFloat64
	if limit, ok := gate.Threshold(); !ok || math.IsInf(limit, 0) {
		t.Fatalf("large finite multiplier produced an unrecordable threshold %g", limit)
	}
}

func TestLatencyGateRequiresUsableHistoryAndFactor(t *testing.T) {
	candidate := sample("candidate", 100, 2, 0)
	candidate.LatencyMS = 1000
	for _, gate := range []LatencyGate{
		{},
		{MeanMS: 100, Samples: 2, Factor: 0},
		{MeanMS: 0, Samples: 2, Factor: 1.5},
		{MeanMS: math.NaN(), Samples: 2, Factor: 1.5},
		{MeanMS: 100, Samples: 2, Factor: math.Inf(1)},
	} {
		if _, ok := gate.Threshold(); ok || gate.Exclusion(candidate) != "" {
			t.Fatalf("invalid gate enabled: %+v", gate)
		}
	}
}
