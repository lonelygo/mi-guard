// Package mihomo provides a deliberately small client for Mihomo's external
// controller API.
package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

const (
	controllerTimeout = 10 * time.Second
	maxErrorBody      = 64 << 10
)

type Client struct {
	base    *url.URL
	secret  string
	http    *http.Client
	initErr error
}

func New(controller, secret string) *Client {
	if !strings.Contains(controller, "://") {
		controller = "http://" + controller
	}
	base, err := url.Parse(controller)
	if err == nil && (base.Scheme != "http" && base.Scheme != "https") {
		err = fmt.Errorf("unsupported controller scheme %q", base.Scheme)
	}
	if err == nil && base.Host == "" {
		err = errors.New("controller address has no host")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{
		base:    base,
		secret:  secret,
		initErr: err,
		http: &http.Client{
			Transport: transport,
			Timeout:   controllerTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *Client) Version(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"version"`
	}
	if err := c.json(ctx, http.MethodGet, "/version", nil, &response); err != nil {
		return "", err
	}
	if response.Version == "" {
		return "", errors.New("controller returned an empty version")
	}
	return response.Version, nil
}

func (c *Client) Proxies(ctx context.Context) (map[string]model.Proxy, error) {
	var response struct {
		Proxies map[string]model.Proxy `json:"proxies"`
	}
	if err := c.json(ctx, http.MethodGet, "/proxies", nil, &response); err != nil {
		return nil, err
	}
	if response.Proxies == nil {
		response.Proxies = make(map[string]model.Proxy)
	}
	return response.Proxies, nil
}

func (c *Client) Proxy(ctx context.Context, name string) (model.Proxy, error) {
	var proxy model.Proxy
	path := "/proxies/" + url.PathEscape(name)
	if err := c.json(ctx, http.MethodGet, path, nil, &proxy); err != nil {
		if statusIs(err, http.StatusNotFound) {
			return c.providerNode(ctx, name)
		}
		return model.Proxy{}, err
	}
	return proxy, nil
}

func (c *Client) Select(ctx context.Context, group, node string) error {
	return c.json(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), struct {
		Name string `json:"name"`
	}{Name: node}, nil)
}

func (c *Client) Delay(ctx context.Context, name, testURL string, timeout time.Duration) (int, error) {
	query := url.Values{}
	query.Set("url", testURL)
	query.Set("expected", "204")
	timeoutMS := timeout.Milliseconds()
	if timeoutMS < 1 {
		timeoutMS = 1
	}
	query.Set("timeout", fmt.Sprintf("%d", timeoutMS))

	var response struct {
		Delay *int `json:"delay"`
	}
	path := "/proxies/" + url.PathEscape(name) + "/delay?" + query.Encode()
	if err := c.json(ctx, http.MethodGet, path, nil, &response); err != nil {
		if !statusIs(err, http.StatusNotFound) {
			return 0, asProbeError(err)
		}
		p, lookupErr := c.providerNode(ctx, name)
		if lookupErr != nil {
			return 0, lookupErr
		}
		path = "/providers/proxies/" + url.PathEscape(p.Provider) + "/" + url.PathEscape(name) + "/healthcheck?" + query.Encode()
		if err = c.json(ctx, http.MethodGet, path, nil, &response); err != nil {
			return 0, asProbeError(err)
		}
	}
	if response.Delay == nil || *response.Delay < 0 {
		return 0, errors.New("invalid controller delay response")
	}
	return *response.Delay, nil
}

// ChainConnections snapshots IDs before changing the upstream selector. Actual
// Mihomo traffic has an outer application tracker, and may have a separate
// Inner tracker for the dialer-proxy underlay. Direct users of the same
// physical node do not belong to either set.
func (c *Client) ChainConnections(ctx context.Context, chain, group, old string) ([]string, error) {
	var response struct {
		Connections []struct {
			ID       string   `json:"id"`
			Chains   []string `json:"chains"`
			Metadata struct {
				Type string `json:"type"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := c.json(ctx, http.MethodGet, "/connections", nil, &response); err != nil {
		return nil, err
	}
	var ids []string
	for _, conn := range response.Connections {
		outer := chain != "" && slices.Contains(conn.Chains, chain)
		inner := strings.EqualFold(conn.Metadata.Type, "inner") && group != "" && old != "" && slices.Contains(conn.Chains, group) && slices.Contains(conn.Chains, old)
		if conn.ID != "" && (outer || inner) {
			ids = append(ids, conn.ID)
		}
	}
	return ids, nil
}

func (c *Client) CloseConnections(ctx context.Context, ids []string) error {
	var failures []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		err := c.json(ctx, http.MethodDelete, "/connections/"+url.PathEscape(id), nil, nil)
		if err != nil && !statusIs(err, http.StatusNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (c *Client) json(ctx context.Context, method, path string, input, output any) error {
	if c.initErr != nil {
		return c.initErr
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode controller request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	endpoint := strings.TrimRight(c.base.String(), "/") + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create controller request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.secret)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("controller %s %s: %w", method, request.URL.EscapedPath(), err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return &responseError{status: response.StatusCode, message: fmt.Sprintf("controller %s %s returned %s: %s", method, request.URL.EscapedPath(), response.Status, strings.TrimSpace(string(message)))}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode controller response: %w", err)
	}
	return nil
}
