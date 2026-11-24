//go:build !cgo

package history

import (
	"context"
	"errors"
	"os"
	"time"
)

func (s *Store) RecordDecision(context.Context, Decision) error { return ErrAnalyticsUnavailable }

func (s *Store) RecentDecisions(context.Context, int) ([]Decision, error) {
	if _, err := os.Stat(s.analyticsPath()); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return nil, ErrAnalyticsUnavailable
}
func (s *Store) DecisionByID(context.Context, string) (Decision, error) {
	if _, err := os.Stat(s.analyticsPath()); errors.Is(err, os.ErrNotExist) {
		return Decision{}, ErrDecisionNotFound
	}
	return Decision{}, ErrAnalyticsUnavailable
}
func (s *Store) addRouteBuckets(context.Context, []RouteBucket, string) error {
	return ErrAnalyticsUnavailable
}
func (s *Store) routeBuckets(context.Context, time.Time, time.Time, RouteInterval) ([]RouteBucket, error) {
	if _, err := os.Stat(s.analyticsPath()); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return nil, ErrAnalyticsUnavailable
}
