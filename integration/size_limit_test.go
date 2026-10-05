package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fixture deliberately omits Content-Length. A decoded-size check after
// host.http.do has buffered the whole response is insufficient: the upstream
// must observe cancellation before it finishes sending its chunked body.
func TestNativeFeedLimitStopsTransfer(t *testing.T) {
	binary, library := nativeInputs(t)
	catalog := fixture(t, "catalog.json")
	recovered := []byte(`{"openai":{"models":{"catalog-fixture":{"cost":{"input":8,"output":40,"cache_read":0.8,"cache_write":10}}}}}`)
	const limit = 1 << 20
	const padding = 4 << 20
	type transfer struct {
		bytes   int
		stopped bool
	}
	finished := make(chan transfer, 1)
	release := make(chan struct{})
	defer close(release)
	var active atomic.Int64
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/catalog.json":
			w.Header().Set("ETag", `"last-good"`)
			_, _ = w.Write(catalog)
		case "/recovered.json":
			w.Header().Set("ETag", `"recovered"`)
			_, _ = w.Write(recovered)
		case "/oversized.json":
			// This would be valid JSON with materially different rates if the
			// complete body were accepted. Padding lives in an ignored field.
			head := `{"openai":{"models":{"catalog-fixture":{"cost":{"input":999,"output":999},"padding":"`
			n, _ := io.WriteString(w, head)
			w.(http.Flusher).Flush()
			sent := n
			chunk := bytes.Repeat([]byte("a"), 32<<10)
			for i := 0; i < padding/len(chunk); i++ {
				select {
				case <-r.Context().Done():
					finished <- transfer{sent, true}
					return
				case <-release:
					return
				case <-time.After(10 * time.Millisecond):
				}
				n, err := w.Write(chunk)
				sent += n
				if err != nil {
					finished <- transfer{sent, true}
					return
				}
				w.(http.Flusher).Flush()
			}
			n, _ = io.WriteString(w, `"}}}}`)
			finished <- transfer{sent + n, false}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(feed.Close)
	h := newHost(t, binary, library, feed.URL)
	raw, err := os.ReadFile(h.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	pricing := cfg["plugins"].(map[string]any)["configs"].(map[string]any)[pluginID].(map[string]any)["pricing"].(map[string]any)
	pricing["feed-max-bytes"] = limit
	pricing["refresh-hours"] = 24
	reconfigure := func(path string) {
		t.Helper()
		pricing["feed-url"] = feed.URL + path
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, h.config, raw)
	}
	reconfigure("/catalog.json")
	h.start()
	type rateResponse struct {
		Feed struct {
			URL, Status string
			ETag        string `json:"etag"`
			Error       string `json:"error"`
		} `json:"feed"`
		Models []struct {
			Status string             `json:"status"`
			ID     string             `json:"rate_card_id"`
			Rates  map[string]float64 `json:"rates"`
		} `json:"models"`
	}
	readRates := func() rateResponse {
		t.Helper()
		r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
		var out rateResponse
		if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil {
			t.Fatalf("rates query failed: %d %s", r.status, r.body)
		}
		return out
	}
	var initial rateResponse
	await(t, "initial valid pricing snapshot", func() bool {
		initial = readRates()
		return initial.Feed.Status == "ok" && len(initial.Models) == 1 && initial.Models[0].Rates["input"] == 4
	})
	reconfigure("/oversized.json")
	select {
	case result := <-finished:
		if !result.stopped || result.bytes >= padding {
			t.Fatalf("host buffered the entire oversized chunked body: sent=%d stopped=%v", result.bytes, result.stopped)
		}
		if result.bytes <= limit {
			t.Fatalf("transfer stopped before the configured cap was exercised: sent=%d limit=%d", result.bytes, limit)
		}
		t.Logf("real upstream transfer stopped after %d bytes for %d-byte cap (would send over %d bytes)", result.bytes, limit, padding)
	case <-time.After(10 * time.Second):
		t.Fatal("oversized feed transfer was not terminated")
	}
	await(t, "oversized upstream work to stop", func() bool { return active.Load() == 0 })
	var rejected rateResponse
	await(t, "feed size error", func() bool { rejected = readRates(); return rejected.Feed.Status == "error" })
	if !strings.Contains(rejected.Feed.Error, "exceed") {
		t.Errorf("feed error does not identify the size limit: %q", rejected.Feed.Error)
	}
	if rejected.Feed.URL != initial.Feed.URL || rejected.Feed.ETag != initial.Feed.ETag || len(rejected.Models) != 1 || rejected.Models[0].ID != initial.Models[0].ID || rejected.Models[0].Rates["input"] != 4 {
		t.Fatalf("oversized feed replaced last-good pricing: %+v", rejected)
	}
	// Same host and plugin; a successful subsequent callback must work after
	// closing the limited stream and releasing its HTTP operation.
	reconfigure("/recovered.json")
	await(t, "valid feed after oversized rejection", func() bool {
		out := readRates()
		return out.Feed.Status == "ok" && out.Feed.URL == feed.URL+"/recovered.json" && out.Feed.ETag == `"recovered"` && len(out.Models) == 1 && out.Models[0].Rates["input"] == 8 && out.Models[0].ID != initial.Models[0].ID
	})
	await(t, "recovered feed request to finish", func() bool { return active.Load() == 0 })
	h.stop(true)
}
