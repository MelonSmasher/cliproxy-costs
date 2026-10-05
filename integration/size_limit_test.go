package integration

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A chunked response must be canceled before the host buffers its whole body.
func TestNativeFeedLimitStopsTransfer(t *testing.T) {
	binary, library := nativeInputs(t)
	f := newOversizedFixture(t)
	defer close(f.release)
	h := newHost(t, binary, library, f.server.URL)
	cfg := readConfig(t, h)
	cfg.pricing["feed-max-bytes"] = feedSizeLimit
	cfg.pricing["refresh-hours"] = 24
	cfg.feedURL(t, f.server.URL+"/catalog.json")
	h.start()
	initial := awaitInitialRates(t, h)
	cfg.feedURL(t, f.server.URL+"/oversized.json")
	assertTransferStopped(t, f)
	await(t, "oversized upstream work to stop", func() bool { return f.active.Load() == 0 })
	assertLastGoodRates(t, h, initial)
	assertSizeLimitRecovery(t, h, cfg, f, initial)
	h.stop(true)
}

func awaitInitialRates(t *testing.T, h *nativeHost) rateResponse {
	t.Helper()
	var initial rateResponse
	await(t, "initial valid pricing snapshot", func() bool {
		initial = readRates(t, h)
		return initial.Feed.Status == "ok" && initial.hasInputRate(4)
	})
	return initial
}

func assertTransferStopped(t *testing.T, f *oversizedFixture) {
	t.Helper()
	select {
	case result := <-f.finished:
		if !result.stopped || result.bytes >= feedPadding {
			t.Fatalf("host buffered the entire oversized chunked body: sent=%d stopped=%v", result.bytes, result.stopped)
		}
		if result.bytes <= feedSizeLimit {
			t.Fatalf("transfer stopped before the configured cap was exercised: sent=%d limit=%d", result.bytes, feedSizeLimit)
		}
		t.Logf("real upstream transfer stopped after %d bytes for %d-byte cap (would send over %d bytes)", result.bytes, feedSizeLimit, feedPadding)
	case <-time.After(10 * time.Second):
		t.Fatal("oversized feed transfer was not terminated")
	}
}

func assertLastGoodRates(t *testing.T, h *nativeHost, initial rateResponse) {
	t.Helper()
	var rejected rateResponse
	await(t, "feed size error", func() bool { rejected = readRates(t, h); return rejected.Feed.Status == "error" })
	if !strings.Contains(rejected.Feed.Error, "exceed") {
		t.Errorf("feed error does not identify the size limit: %q", rejected.Feed.Error)
	}
	if rejected.Feed.URL != initial.Feed.URL || rejected.Feed.ETag != initial.Feed.ETag || !rejected.hasInputRate(4) || rejected.Models[0].ID != initial.Models[0].ID {
		t.Fatalf("oversized feed replaced last-good pricing: %+v", rejected)
	}
}

func assertSizeLimitRecovery(t *testing.T, h *nativeHost, cfg nativeConfig, f *oversizedFixture, initial rateResponse) {
	t.Helper()
	// Same host and plugin must fetch after closing the limited operation.
	cfg.feedURL(t, f.server.URL+"/recovered.json")
	await(t, "valid feed after oversized rejection", func() bool {
		out := readRates(t, h)
		return out.Feed.Status == "ok" && out.Feed.URL == f.server.URL+"/recovered.json" && out.Feed.ETag == `"recovered"` && out.hasInputRate(8) && out.Models[0].ID != initial.Models[0].ID
	})
	await(t, "recovered feed request to finish", func() bool { return f.active.Load() == 0 })
}

const feedSizeLimit = 1 << 20
const feedPadding = 4 << 20

type transfer struct {
	bytes   int
	stopped bool
}
type oversizedFixture struct {
	server   *httptest.Server
	catalog  []byte
	active   atomic.Int64
	finished chan transfer
	release  chan struct{}
}

func newOversizedFixture(t *testing.T) *oversizedFixture {
	t.Helper()
	f := &oversizedFixture{catalog: fixture(t, "catalog.json"), finished: make(chan transfer, 1), release: make(chan struct{})}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *oversizedFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.active.Add(1)
	defer f.active.Add(-1)
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/catalog.json":
		w.Header().Set("ETag", `"last-good"`)
		_, _ = w.Write(f.catalog)
	case "/recovered.json":
		w.Header().Set("ETag", `"recovered"`)
		_, _ = io.WriteString(w, `{"openai":{"models":{"catalog-fixture":{"cost":{"input":8,"output":40,"cache_read":0.8,"cache_write":10}}}}}`)
	case "/oversized.json":
		f.serveOversized(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *oversizedFixture) serveOversized(w http.ResponseWriter, r *http.Request) {
	// A complete response would be valid JSON with changed rates. Padding is in
	// an ignored field. Flushing forces chunked transfer, without Content-Length.
	head := `{"openai":{"models":{"catalog-fixture":{"cost":{"input":999,"output":999},"padding":"`
	sent, _ := io.WriteString(w, head)
	w.(http.Flusher).Flush()
	chunk := bytes.Repeat([]byte("a"), 32<<10)
	for i := 0; i < feedPadding/len(chunk); i++ {
		select {
		case <-r.Context().Done():
			f.finished <- transfer{sent, true}
			return
		case <-f.release:
			return
		case <-time.After(10 * time.Millisecond):
		}
		n, err := w.Write(chunk)
		sent += n
		if err != nil {
			f.finished <- transfer{sent, true}
			return
		}
		w.(http.Flusher).Flush()
	}
	n, _ := io.WriteString(w, `"}}}}`)
	f.finished <- transfer{sent + n, false}
}
