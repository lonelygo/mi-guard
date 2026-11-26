package history

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

const (
	latencyStateVersion    = 1
	latencyRoundsPerSeries = 20
	latencySeriesPerGroup  = 4
)

// LatencyBaseline gives each valid HTTP probe in the retained rounds equal weight.
type LatencyBaseline struct {
	MeanMS  float64
	Samples int
}

// Groups are indexed first by the resolved selector, then by HTTP probe settings.
// A separate file keeps the strictly decoded bandwidth ledger readable by older daemons.
type latencyDiskState struct {
	Version int                                  `json:"version"`
	Groups  map[string]map[string][]latencyRound `json:"groups"`
}

type latencyRound struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	SumMS   float64   `json:"sum_ms"`
	Samples int       `json:"samples"`
}

// LatencyBaseline reads the last atomically published history without creating any files.
func (s *Store) LatencyBaseline(group, probeKey string) (LatencyBaseline, error) {
	if err := validateLatencySeriesKey(group, probeKey); err != nil {
		return LatencyBaseline{}, err
	}
	state, err := s.readLatencyState()
	if err != nil {
		return LatencyBaseline{}, err
	}
	var sum float64
	var count int
	for _, round := range state.Groups[group][probeKey] {
		sum += round.SumMS
		count += round.Samples
	}
	if count == 0 {
		return LatencyBaseline{}, nil
	}
	return LatencyBaseline{MeanMS: sum / float64(count), Samples: count}, nil
}

// RecordLatencyRound merges completed rounds under a cross-process lock. Time
// of observation determines retention even when concurrent writers finish late.
func (s *Store) RecordLatencyRound(ctx context.Context, group, probeKey string, samples []model.Sample) error {
	if err := validateLatencySeriesKey(group, probeKey); err != nil {
		return err
	}
	round := makeLatencyRound(samples)
	if round.Samples == 0 {
		return nil
	}
	return s.WithLock(ctx, "latency", func() error {
		state, err := s.readLatencyState()
		if err != nil {
			return err
		}
		settings := state.Groups[group]
		if settings == nil {
			settings = make(map[string][]latencyRound)
			state.Groups[group] = settings
		}
		rounds := settings[probeKey]
		if slices.ContainsFunc(rounds, func(stored latencyRound) bool { return stored.ID == round.ID }) {
			return nil
		}
		rounds = append(rounds, round)
		slices.SortFunc(rounds, func(a, b latencyRound) int {
			if order := a.At.Compare(b.At); order != 0 {
				return order
			}
			return strings.Compare(a.ID, b.ID)
		})
		settings[probeKey] = rounds[max(0, len(rounds)-latencyRoundsPerSeries):]
		if len(settings) > latencySeriesPerGroup {
			var oldestKey string
			var oldest time.Time
			for key, history := range settings {
				last := history[len(history)-1].At
				if oldestKey == "" || last.Before(oldest) || last.Equal(oldest) && key > oldestKey {
					oldestKey, oldest = key, last
				}
			}
			delete(settings, oldestKey)
		}
		return s.writeHistoryJSON(s.latencyPath(), state)
	})
}

func makeLatencyRound(samples []model.Sample) latencyRound {
	var round latencyRound
	var observations []string
	for _, sample := range samples {
		ms, ok := sample.ProbeLatency()
		if !ok || sample.At.IsZero() {
			continue
		}
		if round.Samples == 0 || sample.At.After(round.At) {
			round.At = sample.At.UTC()
		}
		round.Samples++
		round.SumMS += float64(ms)
		observations = append(observations, fmt.Sprintf("%d/%q/%q/%q/%q/%d",
			sample.At.UnixNano(), sample.Provider, sample.Node, sample.Scope, sample.Kind, ms))
	}
	if round.Samples > 0 {
		// A retry may deliver the same set of probes in a different order.
		slices.Sort(observations)
		round.ID = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(observations, "\n"))))
	}
	return round
}

func (s *Store) latencyPath() string { return s.file + ".latency.json" }

func (s *Store) readLatencyState() (latencyDiskState, error) {
	var state latencyDiskState
	exists, err := readHistoryJSON(s.latencyPath(), &state)
	if err != nil {
		return state, err
	}
	if !exists {
		return latencyDiskState{Version: latencyStateVersion, Groups: make(map[string]map[string][]latencyRound)}, nil
	}
	if state.Version != latencyStateVersion || state.Groups == nil {
		return state, errors.New("invalid latency history version or groups")
	}
	for group, settings := range state.Groups {
		if len(settings) > latencySeriesPerGroup {
			return state, errors.New("invalid number of latency probe settings")
		}
		for key, rounds := range settings {
			if err := validateLatencySeriesKey(group, key); err != nil {
				return state, err
			}
			if len(rounds) < 1 || len(rounds) > latencyRoundsPerSeries {
				return state, errors.New("invalid number of latency rounds")
			}
			seen := make(map[string]bool, len(rounds))
			for i, round := range rounds {
				if round.ID == "" || seen[round.ID] || round.At.IsZero() || round.Samples < 1 || round.SumMS < 0 ||
					(i > 0 && round.At.Before(rounds[i-1].At)) {
					return state, errors.New("invalid latency history round")
				}
				seen[round.ID] = true
			}
		}
	}
	return state, nil
}

func validateLatencySeriesKey(group, probeKey string) error {
	if strings.TrimSpace(group) == "" || strings.TrimSpace(probeKey) == "" {
		return errors.New("latency history needs a group and probe key")
	}
	return nil
}
