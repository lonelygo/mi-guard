package flight

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"time"

	"github.com/lonelygo/mi-guard/internal/history"
)

func calendarBoundaries(t time.Time) []time.Time {
	y, month, day := t.Date()
	today := time.Date(y, month, day, 0, 0, 0, 0, t.Location())
	monday := today.AddDate(0, 0, -(int(today.Weekday())+6)%7)
	monthStart := time.Date(y, month, 1, 0, 0, 0, 0, t.Location())
	points := []time.Time{today, monday, monthStart, t}
	slices.SortFunc(points, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(points, func(a, b time.Time) bool { return a.Equal(b) })
}

func routeHistory(ctx context.Context, store *history.Store, now time.Time) ([]pastRow, error) {
	points := calendarBoundaries(now)
	rows := []pastRow{}
	for i := 1; i < len(points); i++ {
		begin, end := points[i-1], points[i]
		data, err := store.RouteBuckets(ctx, begin, end, history.RoutePeriod)
		if err != nil {
			return nil, err
		}
		for _, v := range data {
			r := route{label: v.Chain, entry: v.Entry, via: v.Exit, provenance: v.RegionSource, target: v.Target}
			rows = append(rows, pastRow{row(r, count{v.UploadBytes, v.DownloadBytes, v.Connections}), begin, end, v.Connections})
		}
	}
	return rows, nil
}

// Display is read-only. One foreground process shares a local lookup cache,
// refreshes live connections every five seconds and re-queries history each minute.
func (m *Monitor) Display(ctx context.Context, store *history.Store, w io.Writer, watch bool) error {
	enc := json.NewEncoder(w)
	var cached []pastRow
	var historyError string
	var refreshed time.Time
	for {
		now := time.Now()
		f := m.frame(ctx, now)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if refreshed.IsZero() || now.Sub(refreshed) >= time.Minute || refreshed.Day() != now.Day() {
			rows, err := routeHistory(ctx, store, now)
			if err != nil {
				historyError = "无法读取航线历史。请检查分析数据库的权限或版本。"
			} else {
				historyError = ""
				cached = rows
			}
			refreshed = now
		}
		if cached != nil {
			f.Buckets = cached
		}
		f.HistoryError = historyError
		if err := enc.Encode(f); err != nil {
			return err
		}
		if !watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
