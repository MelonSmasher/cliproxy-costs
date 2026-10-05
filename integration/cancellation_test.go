package integration

import (
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

// This is an end-to-end cancellation assertion, not just a plugin timeout
// assertion. The real CPA host must close its outstanding upstream HTTP request
// before it is stopped. A callback-name mismatch with a silent legacy fallback
// would return a timeout to the plugin but leave this fixture request alive.
func TestNativeFeedTimeoutCancelsRequest(t *testing.T) {
	binary, library := nativeInputs(t)
	catalog := fixture(t, "catalog.json")
	for _, partialBody := range []bool{false, true} {
		name := "before_headers"
		if partialBody {
			name = "during_body"
		}
		t.Run(name, func(t *testing.T) {
			var active, readyCalls atomic.Int64
			started := make(chan time.Time, 1)
			canceled := make(chan time.Time, 1)
			release := make(chan struct{})
			// Release the fixture before cleanups even on failure, so a broken
			// cancellation path cannot hang httptest.Server.Close or CPA exit.
			defer close(release)
			feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				active.Add(1)
				defer active.Add(-1)
				if r.URL.Path == "/catalog-ready.json" {
					readyCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(catalog)
					return
				}
				if r.URL.Path != "/catalog.json" {
					http.NotFound(w, r)
					return
				}
				select {
				case started <- time.Now():
				default:
				}
				if partialBody {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"openai":{"models":`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
					select {
					case canceled <- time.Now():
					default:
					}
				case <-release:
				}
			}))
			t.Cleanup(feed.Close)
			h := newHost(t, binary, library, feed.URL)
			// Change the temporary config before launch; no host API or transport
			// is replaced. The generous refresh interval prevents retries from
			// obscuring which request was canceled.
			raw, err := os.ReadFile(h.config)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			pricing := cfg["plugins"].(map[string]any)["configs"].(map[string]any)[pluginID].(map[string]any)["pricing"].(map[string]any)
			pricing["feed-timeout-seconds"] = 1
			pricing["refresh-hours"] = 24
			raw, err = json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, h.config, raw)
			h.start()
			var start time.Time
			select {
			case start = <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("host never reached the slow loopback pricing feed")
			}
			select {
			case end := <-canceled:
				elapsed := end.Sub(start)
				if elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
					t.Errorf("upstream cancellation took %s for configured 1-second timeout", elapsed)
				}
				t.Logf("real upstream request canceled after %s (configured timeout 1s)", elapsed)
			case <-time.After(5 * time.Second):
				t.Fatal("plugin timeout did not cancel the actual host HTTP request")
			}
			await(t, "all timed-out upstream work to stop", func() bool { return active.Load() == 0 })
			await(t, "timeout reported by the live plugin", func() bool {
				r := h.request(t, "GET", admin+"rates?models=priced-fixture", managementKey, nil)
				return r.status == http.StatusOK && strings.Contains(string(r.body), "feed fetch timed out after 1s")
			})
			r := h.request(t, "GET", "/v1/models", clientKey, nil)
			if r.status != http.StatusOK {
				t.Fatalf("host stopped instead of canceling one operation: %d %s", r.status, r.body)
			}
			// A URL-only config reload wakes the existing feed worker. This
			// proves the canceled operation releases its callback slot and that
			// the same loaded plugin can fetch again without a process restart.
			pricing["feed-url"] = feed.URL + "/catalog-ready.json"
			raw, err = json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, h.config, raw)
			await(t, "successful next native feed fetch", func() bool {
				r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
				var out struct {
					Feed struct {
						Status string `json:"status"`
						URL    string `json:"url"`
					} `json:"feed"`
					Models []struct {
						Status string             `json:"status"`
						Rates  map[string]float64 `json:"rates"`
					} `json:"models"`
				}
				return json.Unmarshal(r.body, &out) == nil && r.status == http.StatusOK && readyCalls.Load() > 0 && out.Feed.Status == "ok" && out.Feed.URL == feed.URL+"/catalog-ready.json" && len(out.Models) == 1 && out.Models[0].Status == "ok" && out.Models[0].Rates["input"] == 4
			})
			await(t, "successful feed request to finish", func() bool { return active.Load() == 0 })
			h.stop(true)
		})
	}
}
