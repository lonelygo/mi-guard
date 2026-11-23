// Adapted from faceair/clash-speedtest at commit
// 1e1fb9e2bbb29282cce5613a6c6b946b85169fdc, licensed under GPL-3.0.
// Changes include routing through an existing proxy listener, a strict
// application-level byte cap, context cancellation, response validation, and
// correct wall-clock throughput accounting.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

func Download(ctx context.Context, proxyURL, downloadURL string, limit int64, timeout time.Duration) (model.Sample, error) {
	return download(ctx, proxyURL, downloadURL, limit, timeout, nil, false)
}

func DownloadChecked(ctx context.Context, proxyURL, downloadURL string, limit int64, timeout time.Duration, check RouteCheck) (model.Sample, error) {
	return download(ctx, proxyURL, downloadURL, limit, timeout, check, true)
}

func download(ctx context.Context, proxyURL, downloadURL string, limit int64, timeout time.Duration, check RouteCheck, requireCheck bool) (model.Sample, error) {
	sample := model.Sample{At: time.Now().UTC()}
	if limit <= 0 {
		return failedSample(sample, errors.New("download byte limit must be positive"))
	}
	target, err := boundedDownloadURL(downloadURL, limit)
	if err != nil {
		return failedSample(sample, err)
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		return failedSample(sample, fmt.Errorf("parse bounded download URL: %w", err))
	}
	requestContext := ctx
	cancel := func() {}
	if timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	connection, err := openTunnel(requestContext, proxyURL, targetURL)
	if err != nil {
		return failedSample(sample, fmt.Errorf("open download tunnel: %w", err))
	}
	defer connection.Close()
	if requireCheck {
		if err := verifyTunnel(requestContext, connection, check); err != nil {
			return failedSample(sample, err)
		}
	}
	client := clientForTunnel(connection)
	defer client.CloseIdleConnections()

	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, target, nil)
	if err != nil {
		return failedSample(sample, fmt.Errorf("create download request: %w", err))
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=0-%d", limit-1))
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Connection", "close")

	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		setMeasurement(&sample, start)
		if ctx.Err() != nil {
			return failedSample(sample, ctx.Err())
		}
		return failedSample(sample, fmt.Errorf("download request: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		setMeasurement(&sample, start)
		return failedSample(sample, fmt.Errorf("download response returned %s", response.Status))
	}

	sample.Bytes, err = io.CopyN(io.Discard, response.Body, limit)
	setMeasurement(&sample, start)
	if err == nil {
		return sample, nil
	}
	if ctx.Err() != nil {
		return failedSample(sample, ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(requestContext.Err(), context.DeadlineExceeded) && sample.Bytes > 0 {
		// A useful partial sample at the configured deadline is the intended
		// result of a time-boxed speed measurement.
		return sample, nil
	}
	return failedSample(sample, fmt.Errorf("download body ended after %d of %d bytes: %w", sample.Bytes, limit, err))
}

// CheckRoute opens and verifies a tunnel without sending an HTTP request.
func CheckRoute(ctx context.Context, proxyURL, targetURL string, timeout time.Duration, check RouteCheck) error {
	target, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("parse route target URL: %w", err)
	}
	requestContext := ctx
	cancel := func() {}
	if timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	connection, err := openTunnel(requestContext, proxyURL, target)
	if err != nil {
		return fmt.Errorf("open route-check tunnel: %w", err)
	}
	defer connection.Close()
	return verifyTunnel(requestContext, connection, check)
}

func boundedDownloadURL(raw string, limit int64) (string, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse download URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", fmt.Errorf("unsupported download URL scheme %q", target.Scheme)
	}
	if target.Host == "" {
		return "", errors.New("download URL has no host")
	}
	if strings.HasSuffix(strings.TrimRight(target.Path, "/"), "/__down") {
		query := target.Query()
		query.Set("bytes", strconv.FormatInt(limit, 10))
		target.RawQuery = query.Encode()
	}
	return target.String(), nil
}

func setMeasurement(sample *model.Sample, start time.Time) {
	elapsed := time.Since(start)
	sample.ElapsedMS = elapsed.Milliseconds()
	if sample.ElapsedMS < 1 {
		sample.ElapsedMS = 1
	}
	if sample.Bytes <= 0 || elapsed <= 0 {
		return
	}
	sample.Mbps = float64(sample.Bytes*8) / elapsed.Seconds() / 1_000_000
}

func failedSample(sample model.Sample, err error) (model.Sample, error) {
	sample.Error = err.Error()
	return sample, err
}
