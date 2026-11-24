// The bounded parallel-transfer structure in this file is adapted from
// faceair/clash-speedtest commit 1e1fb9e2bbb29282cce5613a6c6b946b85169fdc,
// licensed under GPL-3.0. mi-guard adds one shared quota, per-tunnel route
// proof, a pre-payload barrier, common cancellation, and wall-clock accounting.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

// DownloadBatch downloads one total byte grant over one or more verified
// tunnels. Every stream reaches the route-verification barrier before any HTTP
// payload request starts.
func DownloadBatch(ctx context.Context, proxyURL, downloadURL string, limit int64, timeout time.Duration, streams int, check RouteCheck) (model.Sample, error) {
	if streams == 1 {
		return DownloadChecked(ctx, proxyURL, downloadURL, limit, timeout, check)
	}
	sample := model.Sample{At: time.Now().UTC()}
	if streams < 1 || streams > model.MaxDownloadStreams {
		return failedSample(sample, fmt.Errorf("download streams must be between 1 and %d", model.MaxDownloadStreams))
	}
	if limit < int64(streams) {
		return failedSample(sample, errors.New("download byte limit must provide at least one byte per stream"))
	}

	batchContext, cancel := batchTimeout(ctx, timeout)
	defer cancel()
	barrier := newRouteBarrier(streams)
	results := make([]batchResult, streams)
	var wait sync.WaitGroup
	for index, streamLimit := range splitLimit(limit, streams) {
		wait.Add(1)
		go func(index int, streamLimit int64) {
			defer wait.Done()
			streamCheck := func(checkContext context.Context, connection net.Conn) error {
				if check == nil {
					return fmt.Errorf("%w: missing verifier", ErrRoute)
				}
				if err := check(checkContext, connection); err != nil {
					return err
				}
				return barrier.Wait(checkContext)
			}
			results[index].sample, results[index].err = DownloadChecked(
				batchContext,
				proxyURL,
				downloadURL,
				streamLimit,
				0,
				streamCheck,
			)
			if results[index].err != nil {
				cancel()
			}
		}(index, streamLimit)
	}
	wait.Wait()

	var joined error
	timedOnly := true
	for _, result := range results {
		if result.err != nil && (!errors.Is(result.err, context.DeadlineExceeded) || errors.Is(result.err, ErrRoute)) {
			timedOnly = false
		}
		sample.Bytes += result.sample.Bytes
		joined = errors.Join(joined, result.err)
	}
	if start := barrier.Start(); !start.IsZero() {
		setMeasurement(&sample, start)
	}
	if joined != nil {
		// A planned time-box is a valid aggregate observation once payload was transferred.
		if timedOnly && ctx.Err() == nil && errors.Is(batchContext.Err(), context.DeadlineExceeded) && sample.Bytes > 0 && !barrier.Start().IsZero() {
			return sample, nil
		}
		return failedSample(sample, joined)
	}
	return sample, nil
}

type batchResult struct {
	sample model.Sample
	err    error
}

type routeBarrier struct {
	total int

	mu      sync.Mutex
	arrived int
	start   time.Time
	ready   chan struct{}
}

func newRouteBarrier(total int) *routeBarrier {
	return &routeBarrier{total: total, ready: make(chan struct{})}
}

func (b *routeBarrier) Wait(ctx context.Context) error {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.total {
		b.start = time.Now()
		close(b.ready)
	}
	b.mu.Unlock()

	select {
	case <-b.ready:
		return nil
	case <-ctx.Done():
		// The route proof already passed. Returning nil lets DownloadChecked
		// observe the canceled request context without misclassifying a peer's
		// transport failure as ErrRoute, and no HTTP payload can be sent.
		return nil
	}
}

func (b *routeBarrier) Start() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.start
}

func splitLimit(limit int64, streams int) []int64 {
	limits := make([]int64, streams)
	base := limit / int64(streams)
	remainder := limit % int64(streams)
	for index := range limits {
		limits[index] = base
		if int64(index) < remainder {
			limits[index]++
		}
	}
	return limits
}

func batchTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}
