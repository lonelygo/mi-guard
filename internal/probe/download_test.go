package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDownloadHardCapsIgnoredRangeAndAdjustsDownURL(t *testing.T) {
	t.Parallel()
	const limit = 4096
	var seenRange, seenBytes string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenRange = request.Header.Get("Range")
		seenBytes = request.URL.Query().Get("bytes")
		response.Header().Set("Content-Length", strconv.Itoa(1<<20))
		_, _ = response.Write(make([]byte, 1<<20))
	}))
	defer server.Close()

	sample, err := Download(context.Background(), "", server.URL+"/__down?bytes=999999", limit, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Bytes != limit || sample.Mbps <= 0 {
		t.Fatalf("sample = %#v", sample)
	}
	if seenRange != "bytes=0-4095" || seenBytes != "4096" {
		t.Fatalf("Range=%q bytes=%q", seenRange, seenBytes)
	}
}

func TestDownloadRejectsEarlyBodyTermination(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		hijacker := response.(http.Hijacker)
		connection, buffer, _ := hijacker.Hijack()
		_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n0123456789")
		_ = buffer.Flush()
		_ = connection.Close()
	}))
	defer server.Close()

	sample, err := Download(context.Background(), "", server.URL, 1000, time.Second)
	if err == nil || sample.Bytes != 10 || sample.Error == "" {
		t.Fatalf("sample = %#v, err = %v", sample, err)
	}
}

func TestDownloadUsesPartialSampleAtInternalDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		flusher := response.(http.Flusher)
		response.WriteHeader(http.StatusOK)
		for {
			if _, err := response.Write(make([]byte, 1024)); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer server.Close()

	sample, err := Download(context.Background(), "", server.URL, 1<<30, 35*time.Millisecond)
	if err != nil {
		t.Fatalf("time-boxed sample failed: %v (%#v)", err, sample)
	}
	if sample.Bytes == 0 || sample.Bytes >= 1<<30 || sample.Mbps <= 0 || sample.Error != "" {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestDownloadReportsCallerCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sample, err := Download(ctx, "", "http://127.0.0.1:1/file", 1000, time.Second)
	if !errors.Is(err, context.Canceled) || sample.Error == "" {
		t.Fatalf("sample = %#v, err = %v", sample, err)
	}
}

func TestDownloadDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	_, err := Download(context.Background(), "", redirect.URL, 100, time.Second)
	if err == nil || hits.Load() != 0 {
		t.Fatalf("err = %v, redirect target hits = %d", err, hits.Load())
	}
}

func TestDownloadIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write([]byte("abcd"))
	}))
	defer server.Close()
	if _, err := Download(context.Background(), "", server.URL, 4, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadThroughSOCKS5(t *testing.T) {
	t.Parallel()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write([]byte("through socks"))
	}))
	defer origin.Close()
	proxyURL, closeProxy := startSOCKS5Proxy(t)
	defer closeProxy()

	sample, err := Download(context.Background(), proxyURL, origin.URL, int64(len("through socks")), time.Second)
	if err != nil || sample.Bytes != int64(len("through socks")) {
		t.Fatalf("sample = %#v, err = %v", sample, err)
	}
}

func TestDownloadCheckedProvesAndReusesHTTPConnectTunnel(t *testing.T) {
	t.Parallel()
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		originHits.Add(1)
		_, _ = response.Write([]byte("verified tunnel"))
	}))
	defer origin.Close()
	proxy := startConnectProxy(t)
	defer proxy.Close()

	var checkedPort int
	check := func(_ context.Context, connection net.Conn) error {
		if originHits.Load() != 0 {
			return errors.New("HTTP request arrived before route proof")
		}
		checkedPort = connection.LocalAddr().(*net.TCPAddr).Port
		return nil
	}
	sample, err := DownloadChecked(context.Background(), proxy.URL(), origin.URL, int64(len("verified tunnel")), time.Second, check)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Bytes != int64(len("verified tunnel")) || originHits.Load() != 1 {
		t.Fatalf("sample=%#v origin hits=%d", sample, originHits.Load())
	}
	if proxy.Accepts() != 1 || checkedPort != proxy.LastClientPort() {
		t.Fatalf("accepts=%d checked port=%d proxy client port=%d", proxy.Accepts(), checkedPort, proxy.LastClientPort())
	}
}

func TestDownloadCheckedFailsClosedBeforeHTTPRequest(t *testing.T) {
	t.Parallel()
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		originHits.Add(1)
		_, _ = response.Write([]byte("must not be read"))
	}))
	defer origin.Close()
	proxy := startConnectProxy(t)
	defer proxy.Close()

	sample, err := DownloadChecked(context.Background(), proxy.URL(), origin.URL, 16, time.Second, func(context.Context, net.Conn) error {
		return errors.New("wrong live selector")
	})
	if !errors.Is(err, ErrRoute) || sample.Bytes != 0 || originHits.Load() != 0 {
		t.Fatalf("sample=%#v err=%v origin hits=%d", sample, err, originHits.Load())
	}
}

func TestCheckRouteSendsNoHTTPRequest(t *testing.T) {
	t.Parallel()
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		originHits.Add(1)
	}))
	defer origin.Close()
	proxy := startConnectProxy(t)
	defer proxy.Close()
	checked := false
	err := CheckRoute(context.Background(), proxy.URL(), origin.URL, time.Second, func(context.Context, net.Conn) error {
		checked = true
		return nil
	})
	if err != nil || !checked || originHits.Load() != 0 {
		t.Fatalf("checked=%t err=%v origin hits=%d", checked, err, originHits.Load())
	}
}

func TestCheckRouteIdentifiesRefusedListener(t *testing.T) {
	listener := httptest.NewServer(nil)
	listener.Close()
	for _, proxy := range []string{listener.URL, ""} {
		err := CheckRoute(context.Background(), proxy, listener.URL, time.Second, nil)
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("proxy %q: want connection refused, got %v", proxy, err)
		}
		if strings.Contains(err.Error(), "listener startup or reload log") != (proxy != "") {
			t.Errorf("wrong diagnostic for proxy %q: %v", proxy, err)
		}
	}
}

func TestCheckRouteCancellationUnblocksSilentProxyHandshakes(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			proxyURL, closeProxy := startSilentProxy(t, scheme)
			defer closeProxy()
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(30*time.Millisecond, cancel)
			started := time.Now()
			err := CheckRoute(ctx, proxyURL, "http://127.0.0.1:9/file", time.Hour, func(context.Context, net.Conn) error {
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("CheckRoute error = %v", err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("cancellation took %s", elapsed)
			}
		})
	}
}

func TestGeoDisablesKeepAliveAndParsesTrace(t *testing.T) {
	t.Parallel()
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/geo":
			_, _ = response.Write([]byte(".google.com\n"))
		case "/trace":
			_, _ = response.Write([]byte("ip=203.0.113.1\ncolo=LAX\nloc=US\n"))
		default:
			http.NotFound(response, request)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	result, err := Geo(context.Background(), "", server.URL+"/geo", server.URL+"/trace", ".google.com", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.IP != "203.0.113.1" || result.Colo != "LAX" || result.Loc != "US" {
		t.Fatalf("result = %#v", result)
	}
	if connections.Load() < 2 {
		t.Fatalf("Geo reused a connection, total = %d", connections.Load())
	}
}

func TestGeoCheckedVerifiesBothTunnels(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/geo" {
			_, _ = response.Write([]byte(".google.com"))
			return
		}
		_, _ = response.Write([]byte("ip=203.0.113.2\ncolo=SJC\nloc=US\n"))
	}))
	defer server.Close()
	proxy := startConnectProxy(t)
	defer proxy.Close()
	var checks atomic.Int32
	result, err := GeoChecked(context.Background(), proxy.URL(), server.URL+"/geo", server.URL+"/trace", ".google.com", time.Second, func(context.Context, net.Conn) error {
		checks.Add(1)
		return nil
	})
	if err != nil || !result.OK || checks.Load() != 2 || proxy.Accepts() != 2 {
		t.Fatalf("result=%#v err=%v checks=%d accepts=%d", result, err, checks.Load(), proxy.Accepts())
	}
}

type connectProxy struct {
	listener net.Listener
	accepts  atomic.Int32
	lastPort atomic.Int32
	wait     sync.WaitGroup
}

func startConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &connectProxy{listener: listener}
	proxy.wait.Add(1)
	go proxy.serve()
	return proxy
}

func (p *connectProxy) URL() string         { return "http://" + p.listener.Addr().String() }
func (p *connectProxy) Accepts() int        { return int(p.accepts.Load()) }
func (p *connectProxy) LastClientPort() int { return int(p.lastPort.Load()) }
func (p *connectProxy) Close() {
	_ = p.listener.Close()
	p.wait.Wait()
}

func (p *connectProxy) serve() {
	defer p.wait.Done()
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.accepts.Add(1)
		p.lastPort.Store(int32(connection.RemoteAddr().(*net.TCPAddr).Port))
		p.wait.Add(1)
		go func() {
			defer p.wait.Done()
			defer connection.Close()
			reader := bufio.NewReader(connection)
			request, err := http.ReadRequest(reader)
			if err != nil || request.Method != http.MethodConnect {
				return
			}
			upstream, err := net.Dial("tcp", request.Host)
			if err != nil {
				_, _ = io.WriteString(connection, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
				return
			}
			defer upstream.Close()
			_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
			go func() { _, _ = io.Copy(upstream, reader); _ = upstream.(*net.TCPConn).CloseWrite() }()
			_, _ = io.Copy(connection, upstream)
		}()
	}
}

func startSOCKS5Proxy(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		header := make([]byte, 2)
		if _, err = io.ReadFull(reader, header); err != nil {
			return
		}
		methods := make([]byte, int(header[1]))
		if _, err = io.ReadFull(reader, methods); err != nil {
			return
		}
		_, _ = connection.Write([]byte{0x05, 0x00})
		connect := make([]byte, 4)
		if _, err = io.ReadFull(reader, connect); err != nil {
			return
		}
		host, err := readSOCKSHost(reader, connect[3])
		if err != nil {
			return
		}
		portBytes := make([]byte, 2)
		if _, err = io.ReadFull(reader, portBytes); err != nil {
			return
		}
		target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(portBytes[0])<<8|int(portBytes[1]))))
		if err != nil {
			_, _ = connection.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}
		defer target.Close()
		_, _ = connection.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		go func() { _, _ = io.Copy(target, reader); _ = target.(*net.TCPConn).CloseWrite() }()
		_, _ = io.Copy(connection, target)
	}()
	closeFn := func() {
		_ = listener.Close()
		wait.Wait()
	}
	return (&url.URL{Scheme: "socks5", Host: listener.Addr().String()}).String(), closeFn
}

func readSOCKSHost(reader io.Reader, kind byte) (string, error) {
	switch kind {
	case 0x01:
		value := make([]byte, 4)
		_, err := io.ReadFull(reader, value)
		return net.IP(value).String(), err
	case 0x04:
		value := make([]byte, 16)
		_, err := io.ReadFull(reader, value)
		return net.IP(value).String(), err
	case 0x03:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil {
			return "", err
		}
		value := make([]byte, int(length[0]))
		_, err := io.ReadFull(reader, value)
		return string(value), err
	default:
		return "", fmt.Errorf("unknown address type %d", kind)
	}
}

func startSilentProxy(t *testing.T, scheme string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wait := make(chan struct{})
	go func() {
		defer close(wait)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		buffer := make([]byte, 512)
		_, _ = connection.Read(buffer)
		_, _ = connection.Read(buffer)
	}()
	return scheme + "://" + listener.Addr().String(), func() {
		_ = listener.Close()
		<-wait
	}
}
