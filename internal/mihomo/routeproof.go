package mihomo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

const defaultRouteTimeout = 3 * time.Second

type routePort uint16

func (p *routePort) UnmarshalJSON(data []byte) error {
	value := strings.Trim(string(data), `"`)
	if value == "" || value == "null" {
		*p = 0
		return nil
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		return err
	}
	*p = routePort(parsed)
	return nil
}

type routeConnection struct {
	Chains   []string `json:"chains"`
	Metadata struct {
		SourcePort  routePort `json:"sourcePort"`
		InboundPort routePort `json:"inboundPort"`
	} `json:"metadata"`
}

// VerifyRoute proves that conn is represented by a live controller tracker
// whose exact chain names contain every expected value. The local ephemeral
// port binds the proof to this TCP tunnel; the listener port prevents a stale
// config from proving a different local inbound.
func (c *Client) VerifyRoute(ctx context.Context, conn net.Conn, expected ...string) error {
	if conn == nil {
		return errors.New("route proof has no connection")
	}
	local, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok || local.Port < 1 {
		return fmt.Errorf("route proof needs a TCP local address, got %T", conn.LocalAddr())
	}
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || remote.Port < 1 {
		return fmt.Errorf("route proof needs a TCP remote address, got %T", conn.RemoteAddr())
	}
	if len(expected) == 0 {
		return errors.New("route proof has no expected chain")
	}
	for _, name := range expected {
		if name == "" {
			return errors.New("route proof contains an empty expected chain")
		}
	}

	// Some remote handshakes complete well after the local listener accepts
	// CONNECT. Allow the caller's measurement deadline for the tracker to appear.
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRouteTimeout)
		defer cancel()
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var response struct {
			Connections []routeConnection `json:"connections"`
		}
		if err := c.json(ctx, http.MethodGet, "/connections", nil, &response); err != nil {
			return fmt.Errorf("read live connections for route proof: %w", err)
		}
		for _, candidate := range response.Connections {
			if int(candidate.Metadata.SourcePort) != local.Port {
				continue
			}
			if candidate.Metadata.InboundPort != 0 && int(candidate.Metadata.InboundPort) != remote.Port {
				continue
			}
			for _, name := range expected {
				if !slices.Contains(candidate.Chains, name) {
					quoted := make([]string, len(candidate.Chains))
					for i, chain := range candidate.Chains {
						quoted[i] = model.QuoteName(chain)
					}
					return fmt.Errorf("live tunnel %d used chains [%s], missing %s", local.Port, strings.Join(quoted, ", "), model.QuoteName(name))
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("live tunnel from port %d to inbound %d could not be verified: %w", local.Port, remote.Port, ctx.Err())
		case <-ticker.C:
		}
	}
}
