package mihomo

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lonelygo/mi-guard/internal/model"
)

type responseError struct {
	status  int
	message string
}

func (e *responseError) Error() string { return e.message }
func statusIs(err error, code int) bool {
	var e *responseError
	return errors.As(err, &e) && e.status == code
}

// ProbeUnavailable identifies an actual URL-test timeout or negative result
// returned by Mihomo, as distinct from a missing node or controller outage.
func ProbeUnavailable(err error) bool {
	return statusIs(err, http.StatusServiceUnavailable) || statusIs(err, http.StatusGatewayTimeout)
}

// /proxies exposes global names. Provider leaves also appear in a selector's
// all list, but their details and health checks live under /providers/proxies.
func (c *Client) providerLeaves(ctx context.Context) (map[string]model.Proxy, error) {
	var data struct {
		Providers map[string]struct {
			Vehicle string        `json:"vehicleType"`
			Proxies []model.Proxy `json:"proxies"`
		} `json:"providers"`
	}
	if err := c.json(ctx, http.MethodGet, "/providers/proxies", nil, &data); err != nil {
		return nil, err
	}
	leaves := make(map[string]model.Proxy)
	for name, provider := range data.Providers {
		if provider.Vehicle == "Compatible" {
			continue
		}
		for _, p := range provider.Proxies {
			if old, ok := leaves[p.Name]; ok && old.Provider != name {
				// A name sent to a selector must identify a single effective
				// proxy. Use a non-routable marker for ambiguous provider names.
				leaves[p.Name] = model.Proxy{Name: p.Name, Type: "Ambiguous"}
				continue
			}
			p.Provider = name
			leaves[p.Name] = p
		}
	}
	return leaves, nil
}

func (c *Client) providerNode(ctx context.Context, name string) (model.Proxy, error) {
	leaves, err := c.providerLeaves(ctx)
	if err != nil {
		return model.Proxy{}, err
	}
	p, ok := leaves[name]
	if !ok {
		return p, fmt.Errorf("unknown proxy %q", name)
	}
	if p.Type == "Ambiguous" {
		return p, fmt.Errorf("multiple provider nodes named %q; use distinct provider prefixes", name)
	}
	return p, nil
}

func (c *Client) Inventory(ctx context.Context) (map[string]model.Proxy, error) {
	all, err := c.Proxies(ctx)
	if err != nil {
		return nil, err
	}
	leaves, err := c.providerLeaves(ctx)
	if statusIs(err, http.StatusNotFound) {
		return all, nil
	}
	if err != nil {
		return nil, err
	}
	for name, p := range leaves {
		if _, global := all[name]; !global {
			all[name] = p
		}
	}
	return all, nil
}
