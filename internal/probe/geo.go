package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxProbeBody = 64 << 10

type GeoResult struct {
	Domain string
	IP     string
	Colo   string
	Loc    string
	OK     bool
}

func Geo(ctx context.Context, proxyURL, geoURL, traceURL, expected string, timeout time.Duration) (GeoResult, error) {
	return geo(ctx, proxyURL, geoURL, traceURL, expected, timeout, nil, false)
}

func GeoChecked(ctx context.Context, proxyURL, geoURL, traceURL, expected string, timeout time.Duration, check RouteCheck) (GeoResult, error) {
	return geo(ctx, proxyURL, geoURL, traceURL, expected, timeout, check, true)
}

func geo(ctx context.Context, proxyURL, geoURL, traceURL, expected string, timeout time.Duration, check RouteCheck, requireCheck bool) (GeoResult, error) {
	requestContext := ctx
	cancel := func() {}
	if timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	domain, err := fetchText(requestContext, proxyURL, geoURL, check, requireCheck)
	result := GeoResult{Domain: strings.TrimSpace(domain)}
	if err != nil {
		return result, fmt.Errorf("geo domain probe: %w", err)
	}
	result.OK = result.Domain == expected
	if traceURL == "" {
		return result, nil
	}
	trace, err := fetchText(requestContext, proxyURL, traceURL, check, requireCheck)
	if err != nil {
		return result, fmt.Errorf("Cloudflare trace probe: %w", err)
	}
	for _, line := range strings.Split(trace, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "ip":
			result.IP = value
		case "colo":
			result.Colo = value
		case "loc":
			result.Loc = value
		}
	}
	return result, nil
}

func fetchText(ctx context.Context, proxyURL, address string, check RouteCheck, requireCheck bool) (string, error) {
	target, err := url.Parse(address)
	if err != nil {
		return "", err
	}
	connection, err := openTunnel(ctx, proxyURL, target)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	if requireCheck {
		if err := verifyTunnel(ctx, connection, check); err != nil {
			return "", err
		}
	}
	client := clientForTunnel(connection)
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Connection", "close")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("response returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProbeBody+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxProbeBody {
		return "", errors.New("response exceeds 64 KiB")
	}
	return string(body), nil
}
