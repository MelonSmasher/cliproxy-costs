package abi

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type streamTestHost func(string, []byte) ([]byte, error)

func (h streamTestHost) Call(method string, req []byte) ([]byte, error) { return h(method, req) }

func TestHTTPStreamEnforcesTransferLimitAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int64
		status     int
		chunkError string
		wantBody   string
		wantError  string
		wantReads  int
	}{
		{"exact limit", 4, 200, "", "abcd", "", 2},
		{"over limit", 3, 200, "", "", "response exceeds 3 bytes", 2},
		{"first chunk too large", 1, 200, "", "", "response exceeds 1 bytes", 1},
		{"read error", 4, 200, "upstream reset", "", "upstream reset", 1},
		{"conditional reply", 4, 304, "", "", "", 0},
		{"error page", 4, 503, "", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, closes, cancels := 0, 0, 0
			c := NewHTTPClient(streamTestHost(func(method string, req []byte) ([]byte, error) {
				switch method {
				case "host.http.operation_open":
					return OK(map[string]string{"operation_id": "op1"}), nil
				case "host.http.do_stream":
					if !strings.Contains(string(req), `"operation_id":"op1"`) {
						t.Errorf("missing operation ID: %s", req)
					}
					return OK(map[string]any{"status_code": tc.status, "headers": map[string][]string{"Etag": {"v1"}}, "stream_id": "s1"}), nil
				case "host.http.stream_read":
					if string(req) != `{"stream_id":"s1"}` {
						t.Errorf("invalid stream read: %s", req)
					}
					reads++
					body := "ab"
					if reads == 2 {
						body = "cd"
					}
					return OK(map[string]any{"payload": []byte(body), "done": reads == 2, "error": tc.chunkError}), nil
				case "host.http.stream_close":
					if string(req) != `{"stream_id":"s1"}` {
						t.Errorf("invalid stream close: %s", req)
					}
					closes++
					return OK(nil), nil
				case "host.http.cancel":
					cancels++
					return OK(nil), nil
				default:
					t.Errorf("unexpected callback %s (must not use buffered HTTP)", method)
					return nil, errors.New("unexpected callback")
				}
			}))
			resp, err := c.Do(context.Background(), HostHTTPRequest{Method: "GET", URL: "https://example.invalid/feed"}, tc.limit)
			if tc.wantError == "" {
				if err != nil || resp.StatusCode != tc.status || string(resp.Body) != tc.wantBody || resp.Headers.Get("Etag") != "v1" {
					t.Fatalf("response=%+v error=%v", resp, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || len(resp.Body) != 0 {
				t.Fatalf("response=%+v error=%v, want %q", resp, err, tc.wantError)
			}
			if reads != tc.wantReads || closes != 1 || cancels != 1 {
				t.Fatalf("reads=%d closes=%d cancels=%d", reads, closes, cancels)
			}
		})
	}
}

func TestHTTPStreamCancellationBoundsBlockedReadsAndClose(t *testing.T) {
	readStarted, closeStarted := make(chan struct{}), make(chan struct{})
	releaseRead, releaseClose := make(chan struct{}), make(chan struct{})
	var reads, closes atomic.Int32
	c := NewHTTPClient(streamTestHost(func(method string, _ []byte) ([]byte, error) {
		switch method {
		case "host.http.operation_open":
			return OK(map[string]string{"operation_id": "op1"}), nil
		case "host.http.do_stream":
			return OK(map[string]any{"status_code": 200, "stream_id": "s1"}), nil
		case "host.http.stream_read":
			reads.Add(1)
			close(readStarted)
			<-releaseRead
			return OK(map[string]bool{"done": true}), nil
		case "host.http.stream_close":
			closes.Add(1)
			close(closeStarted)
			<-releaseClose
			return OK(nil), nil
		case "host.http.cancel":
			return OK(nil), nil
		default:
			return nil, errors.New("unexpected callback")
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Do(ctx, HostHTTPRequest{}, 10); !errors.Is(err, context.Canceled) {
			t.Errorf("error=%v, want context canceled", err)
		}
	}()
	awaitHTTP(t, readStarted)
	cancel()
	awaitHTTP(t, done)
	awaitHTTP(t, closeStarted)
	close(releaseRead)
	for range 32 {
		if _, err := c.Do(context.Background(), HostHTTPRequest{}, 10); !errors.Is(err, ErrHTTPBusy) {
			t.Fatalf("retry while close is blocked: %v", err)
		}
	}
	close(releaseClose)
	awaitIdle(t, c)
	if reads.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("reads=%d closes=%d", reads.Load(), closes.Load())
	}
}

func TestHTTPStreamingWorksWithoutOperationCancellationAPI(t *testing.T) {
	closed := false
	c := NewHTTPClient(streamTestHost(func(method string, _ []byte) ([]byte, error) {
		switch method {
		case "host.http.operation_open":
			return Fail("unknown_method", "unsupported"), nil
		case "host.http.do_stream":
			return OK(map[string]any{"status_code": 200, "stream_id": "s1"}), nil
		case "host.http.stream_read":
			return OK(map[string]any{"payload": []byte("ok"), "done": true}), nil
		case "host.http.stream_close":
			closed = true
			return OK(nil), nil
		default:
			t.Errorf("unexpected callback %s", method)
			return nil, errors.New("unexpected callback")
		}
	}))
	resp, err := c.Do(context.Background(), HostHTTPRequest{}, 2)
	if err != nil || string(resp.Body) != "ok" || !closed {
		t.Fatalf("response=%+v error=%v closed=%v", resp, err, closed)
	}
}

func TestHTTPStreamFallbackRequiresExplicitUnsupportedMethod(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reply    []byte
		fallback bool
	}{
		{"unsupported", Fail("unknown_method", "unsupported"), true},
		{"legacy native bridge", Fail("host_call_failed", "unsupported host callback host.http.do_stream"), true},
		{"denied", Fail("forbidden", "denied"), false},
		{"host error", Fail("host_call_failed", "operation failed"), false},
		{"invalid JSON", []byte("not json"), false},
		{"missing stream ID", OK(map[string]any{"status_code": 200}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buffered := false
			c := NewHTTPClient(streamTestHost(func(method string, _ []byte) ([]byte, error) {
				switch method {
				case "host.http.operation_open":
					return Fail("unknown_method", "unsupported"), nil
				case "host.http.do_stream":
					return tc.reply, nil
				case "host.http.do":
					buffered = true
					return OK(HostHTTPResponse{StatusCode: 200, Body: []byte("ok")}), nil
				default:
					return nil, errors.New("unexpected callback")
				}
			}))
			_, err := c.Do(context.Background(), HostHTTPRequest{}, 2)
			if buffered != tc.fallback || (err == nil) != tc.fallback {
				t.Fatalf("buffered=%v error=%v", buffered, err)
			}
		})
	}
}

func TestHTTPLegacyResponseStillRejectsOversize(t *testing.T) {
	c := NewHTTPClient(httpTestHost(func(method string, _ []byte) ([]byte, error) {
		if method == MethodHostHTTPOperationOpen {
			return Fail("unknown_method", "unsupported"), nil
		}
		return OK(HostHTTPResponse{StatusCode: 200, Body: []byte("too large")}), nil
	}))
	if _, err := c.Do(context.Background(), HostHTTPRequest{}, 2); err == nil || !strings.Contains(err.Error(), "exceeds 2 bytes") {
		t.Fatalf("legacy size limit: %v", err)
	}
}
