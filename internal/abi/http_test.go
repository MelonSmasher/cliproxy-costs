package abi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type httpTestHost func(string, []byte) ([]byte, error)

func (h httpTestHost) Call(method string, req []byte) ([]byte, error) {
	if method == "host.http.do_stream" {
		return Fail("unknown_method", "streaming not supported by this fixture"), nil
	}
	return h(method, req)
}

// Wire literals are copied from the supported host SDK, independently of the
// constants under test, so a typo cannot silently exercise legacy fallback.
func TestHTTPOperationMethodWireNames(t *testing.T) {
	if MethodHostHTTPOperationOpen != "host.http.operation_open" || MethodHostHTTPCancel != "host.http.cancel" || MethodHostHTTPDo != "host.http.do" {
		t.Fatal("HTTP operation method names differ from the host SDK")
	}
	if MethodHostHTTPDoStream != "host.http.do_stream" || MethodHostHTTPStreamRead != "host.http.stream_read" || MethodHostHTTPStreamClose != "host.http.stream_close" {
		t.Fatal("HTTP stream method names differ from the host SDK")
	}
}

func awaitHTTP(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host callback")
	}
}

func awaitIdle(t *testing.T, c *HTTPClient) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(c.slot) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("completed HTTP operation did not release its slot")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHTTPClientUsesHostOperationAndCleansUp(t *testing.T) {
	var methods []string
	c := NewHTTPClient(httpTestHost(func(method string, raw []byte) ([]byte, error) {
		methods = append(methods, method)
		switch method {
		case "host.http.operation_open":
			return OK(map[string]string{"operation_id": "op1"}), nil
		case "host.http.do":
			var req HostHTTPRequest
			if err := json.Unmarshal(raw, &req); err != nil || req.OperationID != "op1" || req.URL != "https://example.invalid/feed" {
				t.Errorf("host request: %s (%v)", raw, err)
			}
			return OK(HostHTTPResponse{StatusCode: 200, Body: []byte("ok")}), nil
		case "host.http.cancel":
			if string(raw) != `{"operation_id":"op1"}` {
				t.Errorf("cancel request: %s", raw)
			}
			return OK(nil), nil
		default:
			t.Errorf("unexpected callback %s", method)
			return nil, errors.New("unexpected callback")
		}
	}))
	resp, err := c.Do(context.Background(), HostHTTPRequest{Method: "GET", URL: "https://example.invalid/feed"}, 1<<20)
	if err != nil || resp.StatusCode != 200 || string(resp.Body) != "ok" {
		t.Fatalf("response = %+v, error = %v", resp, err)
	}
	if len(methods) != 3 || methods[0] != MethodHostHTTPOperationOpen || methods[1] != MethodHostHTTPDo || methods[2] != MethodHostHTTPCancel {
		t.Fatalf("callbacks = %v", methods)
	}
}

func TestHTTPClientCancellationCancelsHostAndBoundsRetries(t *testing.T) {
	started, cancelStarted := make(chan struct{}), make(chan struct{})
	releaseDo, releaseCancel := make(chan struct{}), make(chan struct{})
	var doCount, cancelCount atomic.Int32
	c := NewHTTPClient(httpTestHost(func(method string, raw []byte) ([]byte, error) {
		switch method {
		case MethodHostHTTPOperationOpen:
			return OK(map[string]string{"operation_id": "op1"}), nil
		case MethodHostHTTPDo:
			doCount.Add(1)
			close(started)
			<-releaseDo
			return Fail("host_call_failed", "context canceled"), nil
		case MethodHostHTTPCancel:
			cancelCount.Add(1)
			close(cancelStarted)
			<-releaseCancel
			return OK(nil), nil
		default:
			return nil, errors.New("unexpected callback")
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := c.Do(ctx, HostHTTPRequest{}, 1<<20)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want canceled", err)
		}
	}()
	awaitHTTP(t, started)
	cancel()
	awaitHTTP(t, done)
	awaitHTTP(t, cancelStarted)
	for range 32 {
		if _, err := c.Do(context.Background(), HostHTTPRequest{}, 1<<20); !errors.Is(err, ErrHTTPBusy) {
			t.Fatalf("retry error = %v, want busy", err)
		}
	}
	close(releaseDo)
	// The cancel callback is still blocked, so it must also retain the slot.
	if _, err := c.Do(context.Background(), HostHTTPRequest{}, 1<<20); !errors.Is(err, ErrHTTPBusy) {
		t.Fatalf("retry with pending cancel = %v, want busy", err)
	}
	close(releaseCancel)
	awaitIdle(t, c)
	if doCount.Load() != 1 || cancelCount.Load() != 1 {
		t.Fatalf("do=%d cancel=%d", doCount.Load(), cancelCount.Load())
	}
}

func TestHTTPClientLegacyTimeoutDoesNotAccumulateCalls(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	c := NewHTTPClient(httpTestHost(func(method string, _ []byte) ([]byte, error) {
		if method == MethodHostHTTPOperationOpen {
			return Fail("host_call_failed", "unsupported host callback "+method), nil
		}
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return OK(HostHTTPResponse{StatusCode: 200}), nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, HostHTTPRequest{}, 1<<20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	awaitHTTP(t, started)
	for range 32 {
		if _, err := c.Do(context.Background(), HostHTTPRequest{}, 1<<20); !errors.Is(err, ErrHTTPBusy) {
			t.Fatalf("retry error = %v, want busy", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("started %d legacy calls", calls.Load())
	}
	close(release)
	awaitIdle(t, c)
	if _, err := c.Do(context.Background(), HostHTTPRequest{}, 1<<20); err != nil {
		t.Fatalf("retry after callback returned: %v", err)
	}
}

func TestHTTPClientCancelsAnOperationOpenedAfterCancellation(t *testing.T) {
	started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := NewHTTPClient(httpTestHost(func(method string, _ []byte) ([]byte, error) {
		switch method {
		case MethodHostHTTPOperationOpen:
			close(started)
			<-release
			return OK(map[string]string{"operation_id": "late"}), nil
		case MethodHostHTTPCancel:
			close(canceled)
			return OK(nil), nil
		default:
			t.Error("HTTP request started after its context was canceled")
			return nil, errors.New("unexpected callback")
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = c.Do(ctx, HostHTTPRequest{}, 1<<20); close(done) }()
	awaitHTTP(t, started)
	cancel()
	awaitHTTP(t, done)
	close(release)
	awaitHTTP(t, canceled)
	awaitIdle(t, c)
}

func TestHTTPClientFallbackRequiresExplicitUnsupportedMethod(t *testing.T) {
	for _, tc := range []struct {
		name         string
		raw          []byte
		wantFallback bool
	}{
		{"unknown", Fail("unknown_method", "unknown"), true},
		{"missing", Fail("method_not_found", "unknown"), true},
		{"unsupported", Fail("unsupported_method", "unknown"), true},
		{"old host", Fail("host_call_failed", "unsupported host callback "+MethodHostHTTPOperationOpen), true},
		{"permission", Fail("forbidden", "denied"), false},
		{"host failure", Fail("host_call_failed", "host is unavailable"), false},
		{"invalid envelope", []byte(`not json`), false},
		{"missing id", OK(map[string]string{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			c := NewHTTPClient(httpTestHost(func(method string, _ []byte) ([]byte, error) {
				if method == MethodHostHTTPOperationOpen {
					return tc.raw, nil
				}
				called = true
				return OK(HostHTTPResponse{StatusCode: 200}), nil
			}))
			_, err := c.Do(context.Background(), HostHTTPRequest{}, 1<<20)
			if called != tc.wantFallback || (err == nil) != tc.wantFallback {
				t.Fatalf("called=%v error=%v", called, err)
			}
		})
	}
}

func TestHTTPClientAlreadyCanceledDoesNotCallHost(t *testing.T) {
	c := NewHTTPClient(httpTestHost(func(string, []byte) ([]byte, error) {
		t.Error("host called with an already canceled context")
		return nil, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Do(ctx, HostHTTPRequest{}, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
