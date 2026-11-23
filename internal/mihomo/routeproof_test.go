package mihomo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyRoutePollsForMatchingLiveTracker(t *testing.T) {
	t.Parallel()
	connection, closePair := tcpPair(t)
	defer closePair()
	local := connection.LocalAddr().(*net.TCPAddr).Port
	inbound := connection.RemoteAddr().(*net.TCPAddr).Port
	var requests atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) < 3 {
			_, _ = response.Write([]byte(`{"connections":[]}`))
			return
		}
		fmt.Fprintf(response, `{"connections":[{"metadata":{"sourcePort":"%d","inboundPort":"%d"},"chains":["candidate","bench"]}]}`, local, inbound)
	}))
	defer controller.Close()

	if err := New(controller.URL, "").VerifyRoute(context.Background(), connection, "bench", "candidate"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 3 {
		t.Fatalf("proof did not poll, requests=%d", requests.Load())
	}
}

func TestVerifyRouteRejectsMatchingTunnelOnWrongChain(t *testing.T) {
	t.Parallel()
	connection, closePair := tcpPair(t)
	defer closePair()
	local := connection.LocalAddr().(*net.TCPAddr).Port
	inbound := connection.RemoteAddr().(*net.TCPAddr).Port
	controller := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(response, `{"connections":[{"metadata":{"sourcePort":%d,"inboundPort":%d},"chains":["candidate","stale-group"]}]}`, local, inbound)
	}))
	defer controller.Close()

	err := New(controller.URL, "").VerifyRoute(context.Background(), connection, "bench", "candidate")
	if err == nil {
		t.Fatal("wrong live chain was accepted")
	}
}

func TestVerifyRouteHonorsContextWhenTrackerMissing(t *testing.T) {
	t.Parallel()
	connection, closePair := tcpPair(t)
	defer closePair()
	controller := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write([]byte(`{"connections":[]}`))
	}))
	defer controller.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Millisecond)
	defer cancel()
	if err := New(controller.URL, "").VerifyRoute(ctx, connection, "bench"); err == nil {
		t.Fatal("missing tracker was accepted")
	}
}

func tcpPair(t *testing.T) (net.Conn, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		accepted <- connection
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := <-accepted
	return client, func() {
		_ = client.Close()
		_ = server.Close()
		_ = listener.Close()
	}
}
