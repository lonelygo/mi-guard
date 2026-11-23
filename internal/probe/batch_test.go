package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadBatchSplitsOneGrantAcrossThreeIgnoredRanges(t *testing.T) {
	const limit = int64(10)
	var hits atomic.Int32
	var checks atomic.Int32
	var mu sync.Mutex
	var ranges, sizes []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		mu.Lock()
		ranges = append(ranges, request.Header.Get("Range"))
		sizes = append(sizes, request.URL.Query().Get("bytes"))
		mu.Unlock()
		_, _ = response.Write(make([]byte, 1024)) // deliberately ignores Range
	}))
	defer server.Close()

	check := func(context.Context, net.Conn) error {
		if hits.Load() != 0 {
			return errors.New("payload started before all route checks")
		}
		checks.Add(1)
		return nil
	}
	sample, err := DownloadBatch(context.Background(), "", server.URL+"/__down?bytes=999", limit, time.Second, 3, check)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Bytes != limit || sample.Mbps <= 0 || hits.Load() != 3 || checks.Load() != 3 {
		t.Fatalf("sample=%+v hits=%d checks=%d", sample, hits.Load(), checks.Load())
	}
	sort.Strings(ranges)
	sort.Strings(sizes)
	if fmt.Sprint(ranges) != "[bytes=0-2 bytes=0-2 bytes=0-3]" {
		t.Fatalf("ranges = %v", ranges)
	}
	if fmt.Sprint(sizes) != "[3 3 4]" {
		t.Fatalf("bounded query sizes = %v", sizes)
	}
}

func TestDownloadBatchStartsWallClockAfterTwoRouteChecks(t *testing.T) {
	var requestNumber atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if requestNumber.Add(1) == 1 {
			time.Sleep(10 * time.Millisecond)
		} else {
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = response.Write(make([]byte, 100))
	}))
	defer server.Close()

	started := time.Now()
	sample, err := DownloadBatch(context.Background(), "", server.URL, 200, time.Second, 2, func(context.Context, net.Conn) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	})
	wall := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Bytes != 200 || wall < 80*time.Millisecond {
		t.Fatalf("sample=%+v wall=%v", sample, wall)
	}
	if sample.ElapsedMS < 50 || time.Duration(sample.ElapsedMS)*time.Millisecond >= wall-20*time.Millisecond {
		t.Fatalf("measurement includes route-check time: elapsed=%dms wall=%v", sample.ElapsedMS, wall)
	}
	wantMbps := float64(sample.Bytes*8) / (time.Duration(sample.ElapsedMS) * time.Millisecond).Seconds() / 1_000_000
	if sample.Mbps < wantMbps*0.95 || sample.Mbps > wantMbps*1.05 {
		t.Fatalf("Mbps=%v is not based on shared elapsed=%dms", sample.Mbps, sample.ElapsedMS)
	}
}

func TestDownloadBatchRetainsPartialBytesAndJoinsFailure(t *testing.T) {
	const perStream = 10_000
	var requestNumber atomic.Int32
	fullWritten := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if requestNumber.Add(1) == 1 {
			_, _ = response.Write(make([]byte, perStream))
			close(fullWritten)
			return
		}
		<-fullWritten
		time.Sleep(20 * time.Millisecond)
		hijacker := response.(http.Hijacker)
		connection, buffer, _ := hijacker.Hijack()
		_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(perStream) + "\r\nConnection: close\r\n\r\n")
		_, _ = buffer.Write(make([]byte, 1_000))
		_ = buffer.Flush()
		_ = connection.Close()
	}))
	defer server.Close()

	sample, err := DownloadBatch(context.Background(), "", server.URL, 2*perStream, time.Second, 2, func(context.Context, net.Conn) error { return nil })
	if err == nil || sample.Error == "" {
		t.Fatalf("sample=%+v err=%v", sample, err)
	}
	if sample.Bytes < 1_000 || sample.Bytes >= 2*perStream {
		t.Fatalf("partial bytes = %d", sample.Bytes)
	}
}

func TestDownloadBatchVerifierFailureReleasesBarrierBeforePayload(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = response.Write(make([]byte, 100))
	}))
	defer server.Close()
	var checks atomic.Int32
	started := time.Now()
	sample, err := DownloadBatch(context.Background(), "", server.URL, 300, time.Second, 3, func(context.Context, net.Conn) error {
		if checks.Add(1) == 1 {
			return errors.New("wrong selector")
		}
		return nil
	})
	if !errors.Is(err, ErrRoute) || sample.Bytes != 0 || hits.Load() != 0 {
		t.Fatalf("sample=%+v err=%v hits=%d", sample, err, hits.Load())
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("failed verifier left peers blocked at the barrier")
	}
}

func TestDownloadBatchTimeoutAndCallerCancellation(t *testing.T) {
	newSlowServer := func(t *testing.T) (*httptest.Server, <-chan struct{}) {
		t.Helper()
		started := make(chan struct{})
		var once sync.Once
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			once.Do(func() { close(started) })
			flusher := response.(http.Flusher)
			for {
				if _, err := response.Write(make([]byte, 256)); err != nil {
					return
				}
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		}))
		return server, started
	}

	t.Run("timeout", func(t *testing.T) {
		server, _ := newSlowServer(t)
		defer server.Close()
		sample, err := DownloadBatch(context.Background(), "", server.URL, 1<<30, 35*time.Millisecond, 2, func(context.Context, net.Conn) error { return nil })
		if err != nil || sample.Mbps <= 0 || sample.Bytes == 0 || sample.Bytes >= 1<<30 {
			t.Fatalf("sample=%+v err=%v", sample, err)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		server, started := newSlowServer(t)
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-started
			time.Sleep(15 * time.Millisecond)
			cancel()
		}()
		sample, err := DownloadBatch(ctx, "", server.URL, 1<<30, time.Second, 2, func(context.Context, net.Conn) error { return nil })
		if !errors.Is(err, context.Canceled) || sample.Bytes == 0 || sample.Bytes >= 1<<30 {
			t.Fatalf("sample=%+v err=%v", sample, err)
		}
	})
}

func TestDownloadBatchSingleStreamDelegatesAndValidatesBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("abcd"))
	}))
	defer server.Close()
	var checks atomic.Int32
	sample, err := DownloadBatch(context.Background(), "", server.URL, 4, time.Second, 1, func(context.Context, net.Conn) error {
		checks.Add(1)
		return nil
	})
	if err != nil || sample.Bytes != 4 || checks.Load() != 1 {
		t.Fatalf("sample=%+v checks=%d err=%v", sample, checks.Load(), err)
	}
	for _, test := range []struct {
		limit   int64
		streams int
	}{
		{limit: 10, streams: 0},
		{limit: 10, streams: 9},
		{limit: 2, streams: 3},
	} {
		if _, err := DownloadBatch(context.Background(), "", server.URL, test.limit, time.Second, test.streams, func(context.Context, net.Conn) error { return nil }); err == nil {
			t.Fatalf("limit=%d streams=%d was accepted", test.limit, test.streams)
		}
	}
}
