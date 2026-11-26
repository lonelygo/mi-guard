//go:build cgo

package guard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

func TestHardRecoveryDecisionRecordsHistoricalSampleTimeAndLiveValidationOrder(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	f.failing["b"] = true
	var eventID string
	r.NotifyEvent = func(_, _, _, id string) { eventID = id }
	observed := time.Date(2026, time.November, 25, 8, 49, 0, 0, time.UTC)
	ranked := []model.Sample{
		{At: observed, Node: "b", Provider: "provider", Scope: "front", Kind: "sweep", Mbps: 18.4, LatencyMS: 185},
		{At: observed.Add(-time.Minute), Node: "c", Provider: "provider", Scope: "front", Kind: "sweep", Mbps: 14.1, LatencyMS: 260},
		{At: observed.Add(-2 * time.Minute), Node: "d", Provider: "provider", Scope: "front", Kind: "sweep", Mbps: 13.0},
		{At: observed.Add(-3 * time.Minute), Node: "a", Provider: "provider", Scope: "front", Kind: "sweep", Mbps: 12.0},
	}
	results, err := r.applySelections(context.Background(), BenchTarget{
		Group: config.Group{Name: "warp", Chain: "chain"}, Current: "a",
		Evidence: ranked, History: ranked, DecisionTrigger: "hardFailure",
	}, ranked, false)
	if err != nil || len(results) != 1 || results[0].After != "c" || eventID == "" {
		t.Fatalf("recovery = %+v, %v, event %q", results, err, eventID)
	}
	decision, err := r.Store.DecisionByID(context.Background(), eventID)
	if err != nil || decision.Trigger != "hardFailure" || decision.ThresholdMbps != nil || len(decision.Candidates) != 4 {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	failed, selected := decision.Candidates[0], decision.Candidates[1]
	if failed.Kind != "sweep" || failed.ObservedAt == nil || !failed.ObservedAt.Equal(observed) ||
		failed.ValidationOrder != 1 || failed.Validation != "nodeUnavailable" || failed.Selected {
		t.Fatalf("failed candidate = %+v", failed)
	}
	if selected.ValidationOrder != 2 || selected.Validation != "passed" || !selected.Selected ||
		decision.Reason != "按排序完成 2 次在线验证，前 1 次未通过；排序 #2 通过并被采用" {
		t.Fatalf("selected candidate = %+v, decision = %+v", selected, decision)
	}
	if ineligible, notReached := decision.Candidates[2], decision.Candidates[3]; ineligible.Validation != "ineligible" || ineligible.ValidationOrder != 0 ||
		notReached.Validation != "notReached" || notReached.ValidationOrder != 0 {
		t.Fatalf("preliminary and unreached candidates = %+v, %+v", ineligible, notReached)
	}
}

func TestDecisionKeepsPreliminaryFilterSeparateFromSameNodeOnlineFallback(t *testing.T) {
	f, server := newControllerFixture(t)
	r := testRunner(t, f, server)
	r.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) {
		return probe.GeoResult{Domain: ".google.com.hk"}, nil
	}
	bad := model.Sample{Node: "b", Kind: "sweep", Error: "download failed", Failures: 1, Attempts: 1}
	good := model.Sample{Node: "b", Kind: "sweep", Mbps: 20}
	var eventID string
	r.NotifyEvent = func(_, _, _, id string) { eventID = id }
	results, err := r.applySelections(context.Background(), BenchTarget{
		Group:   config.Group{Name: "warp", Chain: "chain", GeoProxy: "fixture"},
		Current: "a", Evidence: []model.Sample{bad, good}, DecisionTrigger: "hardFailure",
	}, []model.Sample{bad, good}, false)
	if err != nil || len(results) != 1 || results[0].After != "b" || eventID == "" {
		t.Fatalf("fallback = %+v, %v, event %q", results, err, eventID)
	}
	decision, err := r.Store.DecisionByID(context.Background(), eventID)
	if err != nil || len(decision.Candidates) != 2 {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	filtered, selected := decision.Candidates[0], decision.Candidates[1]
	if filtered.Validation != "measurementFailed" || filtered.ValidationOrder != 0 || filtered.Selected {
		t.Fatalf("preliminary filter = %+v", filtered)
	}
	if selected.Validation != "fallbackRegionMismatch" || selected.ValidationOrder != 2 || !selected.Selected {
		t.Fatalf("online fallback = %+v", selected)
	}
}

func TestDecisionDistinguishesRegionProbeFailureFromUnexpectedRegion(t *testing.T) {
	for _, tc := range []struct {
		name, validation string
		result           probe.GeoResult
		err              error
	}{
		{name: "probe request failed", validation: "regionProbeFailed", err: errors.New("request timed out")},
		{name: "unexpected region", validation: "regionMismatch", result: probe.GeoResult{Domain: ".google.com.hk"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, server := newControllerFixture(t)
			r := testRunner(t, f, server)
			r.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) { return tc.result, tc.err }
			sample := model.Sample{Node: "b", Kind: "sweep", Mbps: 20}
			_, applyErr := r.applySelections(context.Background(), BenchTarget{
				Group:   config.Group{Name: "warp", Chain: "chain", GeoProxy: "fixture", StrictGeo: true},
				Current: "a", Evidence: []model.Sample{sample}, DecisionTrigger: "hardFailure",
			}, []model.Sample{sample}, false)
			if applyErr == nil {
				t.Fatal("strict region failure was accepted")
			}
			decisions, err := r.Store.RecentDecisions(context.Background(), 1)
			if err != nil || len(decisions) != 1 || len(decisions[0].Candidates) != 1 {
				t.Fatalf("decisions = %+v, %v", decisions, err)
			}
			candidate := decisions[0].Candidates[0]
			if candidate.ValidationOrder != 1 || candidate.Validation != tc.validation || candidate.Selected {
				t.Fatalf("candidate = %+v", candidate)
			}
		})
	}
}

func TestPermissiveRegionFallbackRecordsTheActualAlternateReason(t *testing.T) {
	for _, tc := range []struct {
		name, validation, reason string
		result                   probe.GeoResult
		err                      error
	}{
		{
			name: "probe request failed", validation: "fallbackRegionProbeFailed",
			reason: "地区探测失败，按宽松策略采用首个链路可用候选", err: errors.New("request timed out"),
		},
		{
			name: "unexpected region", validation: "fallbackRegionMismatch",
			reason: "出口地区不符，按宽松策略采用首个链路可用候选",
			result: probe.GeoResult{Domain: ".google.com.hk"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, server := newControllerFixture(t)
			r := testRunner(t, f, server)
			r.geoCheck = func(context.Context, config.Group) (probe.GeoResult, error) { return tc.result, tc.err }
			var eventID string
			r.NotifyEvent = func(_, _, _, id string) { eventID = id }
			sample := model.Sample{Node: "b", Kind: "sweep", Mbps: 20}
			results, err := r.applySelections(context.Background(), BenchTarget{
				Group:   config.Group{Name: "warp", Chain: "chain", GeoProxy: "fixture"},
				Current: "a", Evidence: []model.Sample{sample}, DecisionTrigger: "hardFailure",
			}, []model.Sample{sample}, false)
			if err != nil || len(results) != 1 || results[0].After != "b" || eventID == "" {
				t.Fatalf("fallback = %+v, %v, event %q", results, err, eventID)
			}
			decision, err := r.Store.DecisionByID(context.Background(), eventID)
			if err != nil || decision.Reason != tc.reason || len(decision.Candidates) != 1 {
				t.Fatalf("decision = %+v, %v", decision, err)
			}
			candidate := decision.Candidates[0]
			if candidate.ValidationOrder != 2 || candidate.Validation != tc.validation || !candidate.Selected {
				t.Fatalf("candidate = %+v", candidate)
			}
		})
	}
}
