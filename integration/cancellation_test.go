package integration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The actual host HTTP request must close before CPA stops. A silent legacy
// fallback could time out in the plugin while leaving that request alive.
func TestNativeFeedTimeoutCancelsRequest(t *testing.T) {
	binary, library := nativeInputs(t)
	for _, partial := range []bool{false, true} {
		name := "before_headers"
		if partial {
			name = "during_body"
		}
		t.Run(name, func(t *testing.T) { testNativeTimeout(t, binary, library, partial) })
	}
}

func testNativeTimeout(t *testing.T, binary, library string, partial bool) {
	t.Helper()
	f := newTimeoutFixture(t, partial)
	// Release on failure before cleanup so server.Close and CPA exit cannot hang.
	defer close(f.release)
	h := newHost(t, binary, library, f.server.URL)
	cfg := readConfig(t, h)
	cfg.pricing["feed-timeout-seconds"] = 1
	cfg.pricing["refresh-hours"] = 24
	cfg.save(t)
	h.start()
	assertActualCancellation(t, f)
	await(t, "all timed-out upstream work to stop", func() bool { return f.active.Load() == 0 })
	assertLiveTimeout(t, h)
	assertTimeoutRecovery(t, h, cfg, f)
	h.stop(true)
}

func assertActualCancellation(t *testing.T, f *timeoutFixture) {
	t.Helper()
	var start time.Time
	select {
	case start = <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("host never reached the slow loopback pricing feed")
	}
	select {
	case end := <-f.canceled:
		elapsed := end.Sub(start)
		if elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
			t.Errorf("upstream cancellation took %s for configured 1-second timeout", elapsed)
		}
		t.Logf("real upstream request canceled after %s (configured timeout 1s)", elapsed)
	case <-time.After(5 * time.Second):
		t.Fatal("plugin timeout did not cancel the actual host HTTP request")
	}
}

func assertLiveTimeout(t *testing.T, h *nativeHost) {
	t.Helper()
	await(t, "timeout reported by the live plugin", func() bool {
		r := h.request(t, "GET", admin+"rates?models=priced-fixture", managementKey, nil)
		return r.status == http.StatusOK && strings.Contains(string(r.body), "feed fetch timed out after 1s")
	})
	r := h.request(t, "GET", "/v1/models", clientKey, nil)
	if r.status != http.StatusOK {
		t.Fatalf("host stopped instead of canceling one operation: %d %s", r.status, r.body)
	}
}

func assertTimeoutRecovery(t *testing.T, h *nativeHost, cfg nativeConfig, f *timeoutFixture) {
	t.Helper()
	// URL-only reload wakes the same worker and proves its callback slot is free.
	cfg.feedURL(t, f.server.URL+"/catalog-ready.json")
	await(t, "successful next native feed fetch", func() bool {
		out := readRates(t, h)
		return f.readyCalls.Load() > 0 && out.Feed.Status == "ok" && out.Feed.URL == f.server.URL+"/catalog-ready.json" && out.hasInputRate(4) && out.Models[0].Status == "ok"
	})
	await(t, "successful feed request to finish", func() bool { return f.active.Load() == 0 })
}

type timeoutFixture struct {
	server             *httptest.Server
	catalog            []byte
	partial            bool
	active, readyCalls atomic.Int64
	started, canceled  chan time.Time
	release            chan struct{}
}

func newTimeoutFixture(t *testing.T, partial bool) *timeoutFixture {
	t.Helper()
	f := &timeoutFixture{catalog: fixture(t, "catalog.json"), partial: partial, started: make(chan time.Time, 1), canceled: make(chan time.Time, 1), release: make(chan struct{})}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *timeoutFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.active.Add(1)
	defer f.active.Add(-1)
	if r.URL.Path == "/catalog-ready.json" {
		f.readyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.catalog)
		return
	}
	if r.URL.Path != "/catalog.json" {
		http.NotFound(w, r)
		return
	}
	if f.partial {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"openai":{"models":`)
		w.(http.Flusher).Flush()
	}
	reportTime(f.started)
	select {
	case <-r.Context().Done():
		reportTime(f.canceled)
	case <-f.release:
	}
}

func reportTime(ch chan time.Time) {
	select {
	case ch <- time.Now():
	default:
	}
}
