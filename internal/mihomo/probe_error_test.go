package mihomo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDelayClassifiesOnlyProbeHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		path   string
		status int
		probe  bool
	}{
		{"/proxies/node/delay", http.StatusServiceUnavailable, true},
		{"/providers/proxies/airport/node/healthcheck", http.StatusGatewayTimeout, true},
		{"/providers/proxies", http.StatusGatewayTimeout, false},
		{"/proxies/node/delay", http.StatusUnauthorized, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.path, tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tc.path:
					http.Error(w, "upstream detail", tc.status)
				case "/providers/proxies":
					fmt.Fprint(w, `{"providers":{"airport":{"vehicleType":"HTTP","proxies":[{"name":"node","type":"Trojan"}]}}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			_, err := New(server.URL, "").Delay(context.Background(), "node", "https://example.test/204", time.Second)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%s returned %d", tc.path, tc.status)) {
				t.Fatalf("unexpected underlying error: %v", err)
			}
			status, classified := ProbeStatus(err)
			if classified != tc.probe || (classified && status != tc.status) {
				t.Fatalf("ProbeStatus = %d, %t", status, classified)
			}
		})
	}
}

func TestDelayCancellationIsNotAProbeFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New("http://127.0.0.1:1", "").Delay(ctx, "node", "https://probe.example/204", time.Second)
	if _, ok := ProbeStatus(err); ok {
		t.Fatalf("cancellation was classified as a probe failure: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Delay() error = %v, want context cancellation", err)
	}
}
