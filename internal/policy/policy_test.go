package policy

import (
	"math"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

func TestBaselineUsesHealthyComparableHistory(t *testing.T) {
	samples := []model.Sample{
		sample("node", 100, 10, 0),
		sample("node", 90, 10, 0),
		sample("node", 80, 10, 0),
		sample("node", 2, 10, 0),
		withKind(sample("node", 1000, 10, 0), "quick"),
		withError(sample("node", 999, 10, 0)),
		withScope(sample("node", 999, 10, 0), "chain"),
		sample("other", 999, 10, 0),
	}
	if got := Baseline("node", "provider", samples); got != 85 {
		t.Fatalf("Baseline = %v, want 85", got)
	}
}

func TestDroppedInclusiveBoundary(t *testing.T) {
	for _, test := range []struct {
		name string
		now  float64
		want bool
	}{
		{name: "exactly 80 percent", now: 80, want: true},
		{name: "just above boundary", now: 80.01, want: false},
		{name: "below boundary", now: 20, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Dropped(test.now, 100, 20); got != test.want {
				t.Fatalf("Dropped(%v,100,20) = %v", test.now, got)
			}
		})
	}
	if Dropped(math.NaN(), 100, 20) || Dropped(80, 0, 20) || Dropped(80, 100, 100) {
		t.Fatal("Dropped accepted invalid values")
	}
}

func TestRankUsesReliabilityWithinTopBand(t *testing.T) {
	fresh := []model.Sample{
		sample("fast-lossy", 100, 10, 3),
		sample("stable", 90, 10, 0),
		sample("outside", 79, 10, 0),
	}
	got := Rank("", fresh, nil, 20)
	assertNodes(t, got, "stable", "fast-lossy", "outside")
}

func TestRankUsesHistoricalLowAndThenStickyTie(t *testing.T) {
	fresh := []model.Sample{
		sample("volatile", 100, 10, 0),
		sample("steady", 85, 10, 0),
	}
	past := []model.Sample{
		sample("volatile", 20, 10, 0), sample("volatile", 30, 10, 0), sample("volatile", 100, 10, 0),
		sample("steady", 80, 10, 0), sample("steady", 82, 10, 0), sample("steady", 86, 10, 0),
	}
	assertNodes(t, Rank("volatile", fresh, past, 20), "steady", "volatile")

	closePast := []model.Sample{
		sample("volatile", 80, 10, 0), sample("volatile", 82, 10, 0), sample("volatile", 86, 10, 0),
		sample("steady", 80, 10, 0), sample("steady", 82, 10, 0), sample("steady", 86, 10, 0),
	}
	assertNodes(t, Rank("volatile", fresh, closePast, 20), "volatile", "steady")
}

func TestRankAggregatesHistoricalProbeReliability(t *testing.T) {
	fast := sample("fast", 110, 1, 0)
	steady := sample("steady", 100, 1, 0)
	fastFailureHistory := withError(sample("fast", 0, 100, 30))
	steadyHistory := withError(sample("steady", 0, 100, 1))

	// Other providers, chain probes, and heartbeat metrics must not influence
	// reliability for the measured front route.
	otherProvider := sample("fast", 0, 1000, 0)
	otherProvider.Provider = "other"
	chain := withScope(sample("steady", 0, 1000, 1000), "chain")
	heartbeat := withKind(sample("steady", 0, 1000, 1000), "heartbeat")
	past := []model.Sample{fastFailureHistory, steadyHistory, otherProvider, chain, heartbeat}
	assertNodes(t, Rank("fast", []model.Sample{fast, steady}, past, 20), "steady", "fast")
}

func TestAggregateReliabilityDoesNotDoubleCountFreshSample(t *testing.T) {
	fresh := sample("node", 100, 7, 2)
	fresh.At = time.Date(2026, time.November, 20, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	persisted := fresh
	persisted.At = fresh.At.UTC()
	got := aggregateReliability(fresh, []model.Sample{persisted, persisted})
	if got.attempts != 7 || got.failures != 2 {
		t.Fatalf("aggregate = %+v", got)
	}
}

func TestRankNearBestBoundaryAndPermutationDeterminism(t *testing.T) {
	candidates := []model.Sample{
		sample("best", 100, 10, 0),
		sample("boundary", 80, 10, 1),
		sample("fallback", 79.999, 100, 0),
		sample("slow", 50, 10, 0),
	}
	want := []string{"best", "boundary", "fallback", "slow"}
	for _, permutation := range permutations(candidates) {
		assertNodes(t, Rank("", permutation, nil, 20), want...)
	}
}

func TestRankTreatsMissingReliabilityAsNeutral(t *testing.T) {
	knownGood := sample("known-good", 90, 10, 0)
	unknown := sample("unknown", 95, 0, 0)
	knownBad := sample("known-bad", 100, 10, 8)
	assertNodes(t, Rank("", []model.Sample{unknown, knownGood, knownBad}, nil, 20), "known-good", "unknown", "known-bad")
}

func TestRankRejectsInvalidAndUsesLatestIdentity(t *testing.T) {
	old := sample("same", 100, 1, 0)
	old.At = time.Unix(1, 0)
	latest := sample("same", 85, 1, 0)
	latest.At = time.Unix(2, 0)
	invalid := []model.Sample{
		withError(sample("error", 1000, 10, 0)),
		sample("zero", 0, 10, 0),
		sample("nan", math.NaN(), 10, 0),
	}
	got := Rank("", append([]model.Sample{old, latest}, invalid...), nil, 20)
	if len(got) != 1 || got[0].Mbps != 85 {
		t.Fatalf("Rank = %+v", got)
	}
}

func TestRankUsesLatencyAsFinalTieBreaker(t *testing.T) {
	left := sample("left", 100, 10, 0)
	right := sample("right", 100, 10, 0)
	left.LatencyMS = 200
	right.LatencyMS = 20
	assertNodes(t, Rank("", []model.Sample{left, right}, nil, 20), "right", "left")
}

func sample(node string, mbps float64, attempts, failures int) model.Sample {
	return model.Sample{
		At:        time.Unix(1, 0),
		Node:      node,
		Provider:  "provider",
		Scope:     "front",
		Kind:      "sweep",
		Mbps:      mbps,
		Attempts:  attempts,
		Failures:  failures,
		LatencyMS: 100,
	}
}

func withKind(sample model.Sample, kind string) model.Sample {
	sample.Kind = kind
	return sample
}

func withError(sample model.Sample) model.Sample {
	sample.Error = "failed"
	return sample
}

func withScope(sample model.Sample, scope string) model.Sample {
	sample.Scope = scope
	return sample
}

func assertNodes(t *testing.T, samples []model.Sample, want ...string) {
	t.Helper()
	if len(samples) != len(want) {
		t.Fatalf("got %d samples, want %d: %+v", len(samples), len(want), samples)
	}
	for index, node := range want {
		if samples[index].Node != node {
			t.Fatalf("rank %d = %q, want %q; full=%+v", index, samples[index].Node, node, samples)
		}
	}
}

func permutations(input []model.Sample) [][]model.Sample {
	values := append([]model.Sample(nil), input...)
	var result [][]model.Sample
	var visit func(int)
	visit = func(index int) {
		if index == len(values) {
			result = append(result, append([]model.Sample(nil), values...))
			return
		}
		for next := index; next < len(values); next++ {
			values[index], values[next] = values[next], values[index]
			visit(index + 1)
			values[index], values[next] = values[next], values[index]
		}
	}
	visit(0)
	return result
}
