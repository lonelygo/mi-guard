package mihomo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientCoreOperations(t *testing.T) {
	t.Parallel()
	var selected struct {
		Group string
		Node  string
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(response, "missing authorization", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/version":
			_, _ = response.Write([]byte(`{"version":"v1.19.34"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			_, _ = response.Write([]byte(`{"proxies":{"group":{"name":"group","type":"Selector","now":"node","all":["node"]}}}`))
		case request.Method == http.MethodGet && request.URL.Path == "/proxies/group with/slash":
			_, _ = response.Write([]byte(`{"name":"group with/slash","type":"Selector","now":"node"}`))
		case request.Method == http.MethodPut:
			selected.Group = request.URL.Path[len("/proxies/"):]
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			selected.Node = body.Name
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/proxies/node/delay":
			if request.URL.Query().Get("expected") != "204" || request.URL.Query().Get("timeout") != "3000" {
				http.Error(response, "bad query", http.StatusBadRequest)
				return
			}
			_, _ = response.Write([]byte(`{"delay":42}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := New(server.URL, "test-secret")
	ctx := context.Background()
	version, err := client.Version(ctx)
	if err != nil || version != "v1.19.34" {
		t.Fatalf("Version() = %q, %v", version, err)
	}
	proxies, err := client.Proxies(ctx)
	if err != nil || proxies["group"].Now != "node" {
		t.Fatalf("Proxies() = %#v, %v", proxies, err)
	}
	proxy, err := client.Proxy(ctx, "group with/slash")
	if err != nil || proxy.Name != "group with/slash" {
		t.Fatalf("Proxy() = %#v, %v", proxy, err)
	}
	if err := client.Select(ctx, "🔞 WARP-US", "US / 01"); err != nil {
		t.Fatal(err)
	}
	if selected.Group != "🔞 WARP-US" || selected.Node != "US / 01" {
		t.Fatalf("Select sent %#v", selected)
	}
	delay, err := client.Delay(ctx, "node", "https://example.test/204?a=b", 3*time.Second)
	if err != nil || delay != 42 {
		t.Fatalf("Delay() = %d, %v", delay, err)
	}
}

func TestCleanupUsesPreSwitchIDsForOuterAndInnerTrackers(t *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"connections":[
				{"id":"outer","chains":["warp"]},
				{"id":"underlay","metadata":{"type":"Inner"},"chains":["old","upstream"]},
				{"id":"direct","metadata":{"type":"HTTP"},"chains":["old","direct-group"]},
				{"id":"other","metadata":{"type":"Inner"},"chains":["old","other-upstream"]}
			]}`)
			return
		}
		deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/connections/"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := New(server.URL, "")
	ids, err := c.ChainConnections(context.Background(), "warp", "upstream", "old")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CloseConnections(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []string{"outer", "underlay"}) {
		t.Fatalf("closed %v", deleted)
	}
}

func TestClientDoesNotFollowRedirectOrLeakSecret(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer receiver.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, receiver.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := New(redirector.URL, "private-secret").Version(context.Background())
	if err == nil {
		t.Fatal("Version unexpectedly followed redirect")
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target received %d requests", hits.Load())
	}
}

func TestClientHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(server.URL, "").Version(ctx)
	if err == nil {
		t.Fatal("Version returned no cancellation error")
	}
}

func TestInvalidController(t *testing.T) {
	t.Parallel()
	_, err := New("file:///tmp/controller", "").Version(context.Background())
	if err == nil {
		t.Fatal("invalid controller was accepted")
	}
}

func TestEncodedPathRetainsEscapedSlash(t *testing.T) {
	t.Parallel()
	var rawPath string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rawPath = request.RequestURI
		_, _ = response.Write([]byte(`{"name":"a/b"}`))
	}))
	defer server.Close()
	_, err := New(server.URL, "").Proxy(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if rawPath != "/proxies/a%2Fb" && rawPath != "/proxies/a%2fb" {
		t.Fatalf("RequestURI = %q, expected escaped slash", rawPath)
	}
}

func TestDelayEncodesURL(t *testing.T) {
	t.Parallel()
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		_, _ = response.Write([]byte(`{"delay":1}`))
	}))
	defer server.Close()
	testURL := "https://example.test/a?x=hello world&y=1"
	_, err := New(server.URL, "").Delay(context.Background(), "n", testURL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("url") != testURL {
		t.Fatalf("url query = %q", query.Get("url"))
	}
}
