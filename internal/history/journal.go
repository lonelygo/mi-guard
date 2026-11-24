package history

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	RouteUnknown          = "unknown"
	RouteRegionUnknown    = ""
	RouteRegionConfigured = "configured"
	RouteRegionPeer       = "peer"

	decisionRetention = 1000
	routeRetention    = 93 * 24 * time.Hour
)

var (
	ErrAnalyticsUnavailable = errors.New("analytics database requires cgo and sqlite3")
	ErrDecisionNotFound     = errors.New("decision event not found")
)

// RouteBucket is an observed traffic delta aggregated into a UTC minute. Exit
// identifies the proxy egress region and Target the final destination region.
type RouteBucket struct {
	Start         time.Time `json:"start"`
	Entry         string    `json:"entry,omitempty"`
	Exit          string    `json:"exit"`
	RegionSource  string    `json:"regionSource,omitempty"`
	Target        string    `json:"target"`
	Chain         string    `json:"chain"`
	DownloadBytes int64     `json:"downloadBytes"`
	UploadBytes   int64     `json:"uploadBytes"`
	// Connections counts connections first observed during this interval. It
	// must not contain the full active-connection count from every poll.
	Connections int64 `json:"connections"`
}

type RouteInterval int

const (
	RouteMinute RouteInterval = iota
	RouteHour
	RouteDay
	// RoutePeriod aggregates the complete [since, until) range into one row
	// per route. Callers compute calendar boundaries in the user's location.
	RoutePeriod
)

// AddRouteBuckets records all counter deltas from one controller poll in a
// transaction. sourceID must be stable for that poll, making a retry of the
// entire batch idempotent across crashes and concurrent writers.
func (s *Store) AddRouteBuckets(ctx context.Context, buckets []RouteBucket, sourceID string) error {
	if sourceID = strings.TrimSpace(sourceID); sourceID == "" {
		return errors.New("route bucket source id is missing")
	}
	if len(sourceID) > 256 || strings.ContainsAny(sourceID, "\r\n\x00") {
		return errors.New("route bucket source id is invalid")
	}
	clean := make([]RouteBucket, 0, len(buckets))
	for _, bucket := range buckets {
		if bucket.Start.IsZero() {
			return errors.New("route bucket start time is missing")
		}
		if bucket.DownloadBytes < 0 || bucket.UploadBytes < 0 || bucket.Connections < 0 {
			return errors.New("route bucket counters cannot be negative")
		}
		if bucket.DownloadBytes == 0 && bucket.UploadBytes == 0 && bucket.Connections == 0 {
			continue
		}
		bucket.Start = bucket.Start.UTC().Truncate(time.Minute)
		if strings.TrimSpace(bucket.Entry) != "" {
			bucket.Entry = routeCountry(bucket.Entry)
		}
		bucket.Exit = routeCountry(bucket.Exit)
		if bucket.RegionSource != RouteRegionUnknown && bucket.RegionSource != RouteRegionConfigured && bucket.RegionSource != RouteRegionPeer {
			return errors.New("unsupported route region source")
		}
		bucket.Target = routeCountry(bucket.Target)
		bucket.Chain = routeDimension(bucket.Chain)
		clean = append(clean, bucket)
	}
	sort.Slice(clean, func(i, j int) bool {
		left, right := clean[i], clean[j]
		if !left.Start.Equal(right.Start) {
			return left.Start.Before(right.Start)
		}
		if left.Exit != right.Exit {
			return left.Exit < right.Exit
		}
		if left.Entry != right.Entry {
			return left.Entry < right.Entry
		}
		if left.RegionSource != right.RegionSource {
			return left.RegionSource < right.RegionSource
		}
		if left.Target != right.Target {
			return left.Target < right.Target
		}
		if left.Chain != right.Chain {
			return left.Chain < right.Chain
		}
		if left.DownloadBytes != right.DownloadBytes {
			return left.DownloadBytes < right.DownloadBytes
		}
		if left.UploadBytes != right.UploadBytes {
			return left.UploadBytes < right.UploadBytes
		}
		return left.Connections < right.Connections
	})
	return s.addRouteBuckets(ctx, clean, sourceID)
}

// AddRouteBucket is the single-delta convenience form of AddRouteBuckets.
func (s *Store) AddRouteBucket(ctx context.Context, bucket RouteBucket, sourceID string) error {
	return s.AddRouteBuckets(ctx, []RouteBucket{bucket}, sourceID)
}

// RouteBuckets reads observed buckets in ascending time order, optionally
// rolling minute data into UTC hours or days. A missing database is empty
// history and does not create files.
func (s *Store) RouteBuckets(ctx context.Context, since, until time.Time, interval RouteInterval) ([]RouteBucket, error) {
	if !until.IsZero() && !since.IsZero() && until.Before(since) {
		return nil, errors.New("route bucket time range is reversed")
	}
	if interval < RouteMinute || interval > RoutePeriod {
		return nil, errors.New("unsupported route bucket interval")
	}
	if interval == RoutePeriod && (since.IsZero() || until.IsZero()) {
		return nil, errors.New("route period needs both time boundaries")
	}
	return s.routeBuckets(ctx, since.UTC(), until.UTC(), interval)
}

func routeDimension(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") || strings.Contains(value, "://") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/") {
		return RouteUnknown
	}
	return value
}

func routeCountry(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 2 || value[0] < 'A' || value[0] > 'Z' || value[1] < 'A' || value[1] > 'Z' {
		return RouteUnknown
	}
	return value
}

func (s *Store) analyticsPath() string { return s.file + ".analytics.sqlite3" }
