package policy

import (
	"fmt"
	"math"
	"strings"
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
	if got := Baseline(withKind(sample("node", 20, 1, 0), "quick"), samples); got != 85 {
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
	// These are previous probes, including the one that happened to measure
	// the same speed as the fresh volatile result.
	for i := range past {
		past[i].At = time.Unix(0, int64(i))
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

func TestBenchRankUsesOnlyExactProfileHistory(t *testing.T) {
	for _, mode := range []struct{ streams, other int }{{1, 4}, {4, 1}} {
		t.Run(fmt.Sprintf("%d-stream", mode.streams), func(t *testing.T) {
			current := withKind(sample("current", 90, 2, 0), "bench")
			current.Profile.Streams = mode.streams
			peer := current
			peer.Node, peer.Mbps = "peer", 100
			fresh := []model.Sample{current, peer}

			// Two samples are insufficient for a quartile. A third comparable
			// bandwidth or a failed probe of the current node breaks the sticky tie.
			var bandwidth []model.Sample
			for i := 0; i < 3; i++ {
				old := peer
				old.At, old.Mbps, old.Attempts = time.Unix(int64(i), 0), 1000, 0
				bandwidth = append(bandwidth, old)
			}
			failure := withError(current)
			failure.Mbps, failure.Attempts, failure.Failures = 0, 10, 10
			assertNodes(t, Rank("current", fresh, bandwidth[:2], 20), "current", "peer")
			assertNodes(t, Rank("current", fresh, bandwidth, 20), "peer", "current")
			assertNodes(t, Rank("current", fresh, []model.Sample{failure}, 20), "peer", "current")
			both := append([]model.Sample(nil), bandwidth...)
			for _, old := range bandwidth {
				old.Node, old.Mbps = current.Node, 50
				both = append(both, old)
			}
			assertNodes(t, Rank("current", fresh, both, 20), "peer", "current")

			for _, tc := range []struct {
				name   string
				change func(*model.Sample)
			}{
				{"opposite stream count", func(s *model.Sample) { s.Profile.Streams = mode.other }},
				{"different requested bytes", func(s *model.Sample) { s.Profile.TotalBytes++ }},
				{"different target", func(s *model.Sample) { s.Profile.Target = strings.Repeat("b", 64) }},
				{"legacy", func(s *model.Sample) { s.Profile = model.DownloadProfile{} }},
				{"daemon", func(s *model.Sample) { s.Kind = "sweep" }},
				{"different provider", func(s *model.Sample) { s.Provider = "former" }},
				{"different route", func(s *model.Sample) { s.Scope = "chain:" + s.Node }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					past := append([]model.Sample{failure}, bandwidth...)
					for i := range past {
						tc.change(&past[i])
					}
					assertNodes(t, Rank("current", fresh, past, 20), "current", "peer")
				})
			}
		})
	}
}

func TestUnknownBenchProfileUsesFreshEvidenceAlone(t *testing.T) {
	current := withKind(sample("current", 100, 2, 1), "bench")
	current.Profile = model.DownloadProfile{}
	peer := current
	peer.Node, peer.Mbps, peer.Failures = "peer", 90, 0
	old := withError(peer)
	old.Mbps, old.Attempts, old.Failures = 0, 100, 100
	assertNodes(t, Rank("current", []model.Sample{current, peer}, []model.Sample{old}, 20), "peer", "current")
}

func TestChainBenchUsesItsOwnHistoryAndFreshProbes(t *testing.T) {
	current := withScope(withKind(sample("current", 95, 2, 0), "bench"), "chain:current")
	peer := withScope(withKind(sample("peer", 100, 2, 0), "bench"), "chain:peer")
	fresh := []model.Sample{current, peer}
	failure := withError(peer)
	failure.Mbps, failure.Failures = 0, 2
	assertNodes(t, Rank("peer", fresh, []model.Sample{withScope(failure, "front")}, 20), "peer", "current")
	assertNodes(t, Rank("peer", fresh, []model.Sample{failure}, 20), "current", "peer")
	fresh[1].Failures = 1
	assertNodes(t, Rank("peer", fresh, nil, 20), "current", "peer")
}

func TestDaemonStatsShareProbeSizesAndExcludeOtherProfiles(t *testing.T) {
	quick := withKind(sample("node", 75, 2, 0), "quick")
	quick.Profile.TotalBytes = 256
	sweep := sample("node", 80, 2, 0)
	confirm := withKind(sample("node", 100, 2, 0), "confirm")
	confirm.Profile.TotalBytes = 1024
	failure := withError(quick)
	failure.Mbps, failure.Failures = 0, 2

	high := sample("node", 3000, 10, 5)
	multi, legacy, partial, target := high, high, high, high
	multi.Profile.Streams = 4
	legacy.Profile = model.DownloadProfile{}
	partial.Profile.TotalBytes = 0
	target.Profile.Target = strings.Repeat("b", 64)
	past := []model.Sample{sweep, confirm, failure, withKind(high, "bench"), multi, legacy, partial, target}
	stats := statsFor(quick, past)
	if stats.attempts != 8 || stats.failures != 2 || len(stats.bandwidth) != 2 {
		t.Fatalf("daemon stats = %+v", stats)
	}
	if got := Baseline(quick, past); got != 90 {
		t.Fatalf("quick baseline = %v, want 90", got)
	}
}

func TestDuplicateObservationsIncludeTheirDownloadProfile(t *testing.T) {
	fresh := sample("node", 100, 7, 2)
	fresh.At = time.Date(2026, time.November, 20, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	persisted := fresh
	persisted.At = fresh.At.UTC()
	otherSize := persisted
	otherSize.Profile.TotalBytes++
	got := statsFor(fresh, []model.Sample{persisted, persisted, otherSize})
	if got.attempts != 14 || got.failures != 4 || len(got.bandwidth) != 2 {
		t.Fatalf("distinct observations = %+v", got)
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
	if threshold, known := PreferredThreshold(append([]model.Sample{old, latest}, invalid...), 20); !known || threshold != 68 {
		t.Fatalf("PreferredThreshold = %v, %v; want 68, true", threshold, known)
	}
}

func TestRankUsesLatencyAsFinalTieBreaker(t *testing.T) {
	left := sample("left", 100, 10, 0)
	right := sample("right", 100, 10, 0)
	left.LatencyMS = 200
	right.LatencyMS = 20
	assertNodes(t, Rank("", []model.Sample{left, right}, nil, 20), "right", "left")
}

func TestExplainHistoryUsesTheSameMethodBoundaryAsRanking(t *testing.T) {
	fresh := withKind(sample("node", 90, 2, 0), "bench")
	fresh.Profile = model.DownloadProfile{Streams: 4, TotalBytes: 16 << 20, Target: strings.Repeat("a", 64)}
	past := []model.Sample{}
	for index, speed := range []float64{60, 80, 100} {
		old := fresh
		old.At = time.Unix(int64(index+2), 0)
		old.Mbps = speed
		old.Failures = index % 2
		past = append(past, old)
	}
	wrongStreams := fresh
	wrongStreams.At = time.Unix(10, 0)
	wrongStreams.Profile.Streams = 1
	wrongStreams.Mbps, wrongStreams.Attempts, wrongStreams.Failures = 999, 100, 100
	wrongTarget := fresh
	wrongTarget.At = time.Unix(11, 0)
	wrongTarget.Profile.Target = strings.Repeat("b", 64)
	wrongTarget.Mbps, wrongTarget.Attempts, wrongTarget.Failures = 999, 100, 100
	past = append(past, wrongStreams, wrongTarget)

	evidence := ExplainHistory(fresh, past)
	if evidence.BandwidthCount != 3 || !evidence.P25Known || evidence.P25Mbps != 70 || evidence.Attempts != 8 || evidence.Failures != 1 ||
		evidence.PastAttempts != 6 || evidence.PastFailures != 1 {
		t.Fatalf("bench evidence mixed measurement methods: %+v", evidence)
	}

	quick := withKind(sample("node", 40, 1, 0), "quick")
	sweep := withKind(sample("node", 50, 2, 0), "sweep")
	sweep.At = time.Unix(20, 0)
	confirm := withKind(sample("node", 70, 2, 1), "confirm")
	confirm.At = time.Unix(21, 0)
	daemonEvidence := ExplainHistory(quick, []model.Sample{sweep, confirm, fresh})
	if daemonEvidence.BandwidthCount != 2 || daemonEvidence.P25Known || daemonEvidence.Attempts != 5 || daemonEvidence.Failures != 1 ||
		daemonEvidence.PastAttempts != 4 || daemonEvidence.PastFailures != 1 {
		t.Fatalf("daemon evidence mixed manual bench history: %+v", daemonEvidence)
	}
}

func sample(node string, mbps float64, attempts, failures int) model.Sample {
	return model.Sample{
		At:        time.Unix(1, 0),
		Node:      node,
		Provider:  "provider",
		Scope:     "front",
		Kind:      "sweep",
		Profile:   model.DownloadProfile{Streams: 1, TotalBytes: 4096, Target: strings.Repeat("a", 64)},
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
